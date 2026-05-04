package mcphandlers

// enju_execute_run — drain all ready compute tasks in a run
// without requiring the caller to push enju_execute_task for
// each one. Citizen tasks (vote, review, answer, contribute)
// are never auto-advanced; the cascade stops at the next human
// gate and reports it as next_blocker.
//
// This file is now a thin transport-layer translator: parses
// MCP args, forwards to service.FatClient.ExecuteRun, formats
// the structured cascade result back to MCP text. All
// orchestration (serial loop, parallel dispatch, claim retry,
// compute execution, cold-reconcile fallback) lives in
// internal/fatclient/service/execute_run.go.

import (
	"context"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/service"
	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultExecuteRunLimit = 100
	maxExecuteRunLimit     = 1000
	// Parallel-dispatch knob. Default 4 (sweet spot for the
	// typical bio fan-out: 4-way judges, 4-way alignment
	// pipelines), cap at 32 for power users with beefy hosts.
	defaultParallel = 4
	maxParallel     = 32
)

func (c *apiClient) handleExecuteRun(ctx context.Context, req mcp.CallToolRequest) (*mcp.CallToolResult, error) {
	projectID, err := req.RequireInt("project_id")
	if err != nil {
		return mcp.NewToolResultError("project_id is required"), nil
	}
	runID, err := req.RequireInt("run_id")
	if err != nil {
		return mcp.NewToolResultError("run_id is required"), nil
	}
	if c.fc.Workspace() == nil {
		return mcp.NewToolResultError("enju_execute_run requires a local workspace"), nil
	}
	maxTasks := req.GetInt("max_tasks", defaultExecuteRunLimit)
	if maxTasks <= 0 {
		maxTasks = defaultExecuteRunLimit
	}
	if maxTasks > maxExecuteRunLimit {
		return mcp.NewToolResultError(fmt.Sprintf(
			"max_tasks %d exceeds hard cap %d — lower it, or split the cascade across calls",
			maxTasks, maxExecuteRunLimit)), nil
	}
	parallel := req.GetInt("parallel", defaultParallel)
	if parallel <= 0 {
		parallel = defaultParallel
	}
	if parallel > maxParallel {
		return mcp.NewToolResultError(fmt.Sprintf(
			"parallel %d exceeds hard cap %d — diminishing returns past the proj.Lock contention point on git commit/push, and bio scripts can be RAM-heavy. If you genuinely need more, split the cascade across multiple enju_execute_run calls.",
			parallel, maxParallel)), nil
	}

	res, err := c.fc.ExecuteRun(ctx, service.ExecuteRunParams{
		ProjectID: projectID,
		RunID:     runID,
		MaxTasks:  maxTasks,
		Parallel:  parallel,
	})
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}
	return mcp.NewToolResultText(formatExecuteRunSummary(res.Entries, res.StopReason, res.Blocker, maxTasks, parallel)), nil
}

// formatExecuteRunSummary produces the human-readable response.
// Header first (N completed / M failed / stop reason), then one
// line per executed entry. Matches the shape of
// formatClaimMatchingSummary + formatBatchSubmit so a caller
// that parses batch responses can key off the same structure.
func formatExecuteRunSummary(entries []service.ExecuteRunEntry, stopReason string, blocker *service.ExecuteRunBlocker, maxTasks, parallel int) string {
	var completed, failed, errored, async, gitFailed int
	for _, e := range entries {
		switch e.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		case "git_failed":
			gitFailed++
		case "async_started":
			async++
		case "error":
			errored++
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Cascade: %d executed", len(entries)))
	if completed > 0 || failed > 0 || gitFailed > 0 || async > 0 || errored > 0 {
		parts := []string{}
		if completed > 0 {
			parts = append(parts, fmt.Sprintf("%d completed", completed))
		}
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", failed))
		}
		if gitFailed > 0 {
			parts = append(parts, fmt.Sprintf("%d git-op-failed", gitFailed))
		}
		if async > 0 {
			parts = append(parts, fmt.Sprintf("%d async started", async))
		}
		if errored > 0 {
			parts = append(parts, fmt.Sprintf("%d errored", errored))
		}
		b.WriteString(" (")
		b.WriteString(strings.Join(parts, ", "))
		b.WriteString(")")
	}
	b.WriteString("\n")

	b.WriteString(fmt.Sprintf("Stop reason: %s", stopReason))
	switch stopReason {
	case service.StopCitizenTaskReady:
		if blocker != nil {
			b.WriteString(fmt.Sprintf(" — next_blocker=%s (action=%s)", blocker.TaskID, blocker.Action))
		}
		b.WriteString("\n  → call enju_claim_task for this task; after submitting, call enju_execute_run again to resume.\n")
	case service.StopComputeAssignedElsewhere:
		if blocker != nil {
			b.WriteString(fmt.Sprintf(" — next_blocker=%s (action=compute, assigned to another citizen)", blocker.TaskID))
		}
		b.WriteString("\n  → wait for the assigned citizen to execute this task, then call enju_execute_run again.\n")
	case service.StopNoReadyCompute:
		b.WriteString("\n  → run is idle or complete; check enju_run_status.\n")
	case service.StopComputeFailed:
		if parallel > 1 {
			// In parallel mode, in-flight siblings drain
			// before the cascade returns (complete-then-stop
			// policy: bio scripts may not be safely
			// interruptible mid-run). Surface so operators
			// don't think they're stuck. Ctrl-C still cancels
			// everything via ctx.
			b.WriteString(fmt.Sprintf(" — in-flight siblings drained (parallel=%d, complete-then-stop policy; Ctrl-C cancels via ctx)", parallel))
		}
		b.WriteString("\n  → downstream tasks blocked. Fix the script + enju_invalidate_task, or accept the failure.\n")
	case service.StopGitOperationFailed:
		// Don't suggest enju_invalidate_task here. The task is
		// still in `claimed` state (no /result, no /fail
		// reported to the coordinator), so invalidate would
		// reject. ExecuteComputeTask's claim gate already
		// handles the "claimed by us" retry path.
		if parallel > 1 {
			b.WriteString(fmt.Sprintf(" — in-flight siblings drained (parallel=%d)", parallel))
		}
		b.WriteString("\n  → script ran fine — work product is still on disk. Failure was at the git layer (commit/push). Inspect enju_project_remote_status, fix the remote state, then call enju_execute_run again to retry.\n")
	case service.StopAsyncTaskStarted:
		b.WriteString("\n  → an async compute task is running detached. Call enju_execute_run again once it lands.\n")
	case service.StopMaxTasks:
		b.WriteString(fmt.Sprintf(" — hit max_tasks=%d; call again to continue.\n", maxTasks))
	case service.StopContextCancelled:
		b.WriteString(" — caller cancelled the request.\n")
	default:
		b.WriteString("\n")
	}

	if len(entries) > 0 {
		b.WriteString("\n")
		for _, e := range entries {
			writeExecuteRunEntryLine(&b, e)
		}
	}
	return b.String()
}

func writeExecuteRunEntryLine(b *strings.Builder, e service.ExecuteRunEntry) {
	var prefix string
	switch e.Status {
	case "completed":
		prefix = "✓"
	case "failed":
		prefix = "✗"
	case "git_failed":
		prefix = "✗git"
	case "async_started":
		prefix = "…"
	default:
		prefix = "!"
	}
	b.WriteString(fmt.Sprintf("  %s %s", prefix, e.TaskID))
	if e.Script != "" {
		b.WriteString(fmt.Sprintf(" [%s]", e.Script))
	}
	if e.ElapsedMS > 0 {
		b.WriteString(fmt.Sprintf(" (%dms)", e.ElapsedMS))
	}
	if e.CommitSHA != "" {
		b.WriteString(fmt.Sprintf(" commit=%s", format.ShortSHA(e.CommitSHA)))
	}
	if len(e.Artifacts) > 0 {
		b.WriteString(fmt.Sprintf(" artifacts=%s", strings.Join(e.Artifacts, ",")))
	}
	if e.Reason != "" {
		reason := e.Reason
		if len(reason) > 200 {
			reason = reason[:200] + "...(truncated)"
		}
		b.WriteString(fmt.Sprintf(" — %s", reason))
	}
	b.WriteString("\n")
}
