package mcpserver

// enju_execute_run — drain all ready compute tasks in a run
// without requiring the caller to push enju_execute_task for
// each one. Citizen tasks (vote, review, answer, contribute)
// are never auto-advanced; the cascade stops at the next human
// gate and reports it as next_blocker so the operator knows
// where the pipeline paused.
//
// Why a batch tool (not a run-level "auto_compute" flag):
//   - Composes with single-task flows. An AI-citizen submits
//     their vote, then calls execute_run to flush the
//     deterministic work their submission unblocked. No mode
//     state to track.
//   - Natural extension surface. A future `filter=` param can
//     scope the drain to a sub-DAG (parallel arms, partial
//     re-execution) without a new tool.
//   - If operators eventually want fire-and-forget runs,
//     a daemon that calls execute_run on a timer is a trivial
//     add — the mode is just this tool in a loop.
//
// Serial-only for v1: ready compute tasks execute one-at-a-time
// within this call. Two ready fan-out judges run back-to-back,
// not in parallel. Parallel execution would need per-task
// workspaces or careful git-lock sequencing — the cost isn't
// justified until a real workload shows serial is the
// bottleneck (the typical case is the run is I/O-bound on
// script execution, which overlapping workspace writes would
// just contend over). See TODO.md for the parallel upgrade if
// demand materializes.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

const (
	defaultExecuteRunLimit = 100
	maxExecuteRunLimit     = 1000
)

// executeRunEntry captures one task's outcome in the batch
// cascade. Separate from executeOutcome because the batch
// response wants a lean per-entry line without the full
// structured payload each step emits.
type executeRunEntry struct {
	TaskID    string `json:"task_id"`
	Status    string `json:"status"` // "completed" | "failed" | "async_started" | "error"
	Script    string `json:"script,omitempty"`
	ElapsedMS int64  `json:"elapsed_ms,omitempty"`
	CommitSHA string `json:"commit_sha,omitempty"`
	Reason    string `json:"reason,omitempty"` // error message, stderr summary, etc.
	Artifacts []string `json:"artifacts,omitempty"`
}

type executeRunBlocker struct {
	TaskID string `json:"task_id"`
	Action string `json:"action"`
}

// Stop reasons for the cascade loop. Surfaced verbatim to
// callers so they can script behavior per reason without
// substring-matching free text.
const (
	stopNoReadyCompute           = "no_ready_compute"
	stopCitizenTaskReady         = "citizen_task_ready"
	stopComputeAssignedElsewhere = "compute_assigned_elsewhere"
	stopComputeFailed    = "compute_failed"
	stopComputeErrored   = "compute_errored"
	stopAsyncTaskStarted = "async_task_started"
	stopMaxTasks         = "max_tasks"
	stopContextCancelled = "context_cancelled"
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
	if c.workspace == nil {
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

	// Pre-flight: resolve the run. Gives us a real "run not
	// found" error (vs. the misleading "idle run" the main loop
	// would produce for an empty /ready on a nonexistent run)
	// and caches the branch for the cold-reconcile fallback
	// below.
	runBranch, err := c.fetchRunBranch(ctx, projectID, runID)
	if err != nil {
		return mcp.NewToolResultError(err.Error()), nil
	}

	var entries []executeRunEntry
	var stopReason string
	var blocker *executeRunBlocker
	// coldReconcileTried guards against repeatedly reconciling
	// when nothing lands — we only want the "came back after an
	// overnight async completion" recovery path to fire once
	// per call.
	coldReconcileTried := false

	for len(entries) < maxTasks {
		if err := ctx.Err(); err != nil {
			stopReason = stopContextCancelled
			break
		}

		ready, err := c.fetchReadyTasksForRun(ctx, projectID, runID)
		if err != nil {
			// Fold into the response as a terminal error rather
			// than ToolResultError — we want partial results to
			// reach the caller even if the final /ready fetch
			// fails mid-cascade.
			entries = append(entries, executeRunEntry{
				Status: "error",
				Reason: "fetch ready tasks: " + err.Error(),
			})
			stopReason = stopComputeErrored
			break
		}

		// Cold-start reconcile. If we're called without a recent
		// submit in this session (e.g. an overnight async job
		// just landed), the coordinator's state for this run
		// may still reflect pre-completion. An empty /ready on
		// the first scan is ambiguous between "nothing to do"
		// and "stale state" — do a pull-and-reconcile once to
		// disambiguate before concluding the run is idle.
		if !coldReconcileTried && len(entries) == 0 && len(ready) == 0 && runBranch != "" {
			coldReconcileTried = true
			if proj, _, _, _, perr := c.openProject(ctx, int64(projectID)); perr == nil && proj != nil {
				_ = c.pullBranchWithReconcile(ctx, proj, int64(projectID), runBranch)
				ready, err = c.fetchReadyTasksForRun(ctx, projectID, runID)
				if err != nil {
					entries = append(entries, executeRunEntry{
						Status: "error",
						Reason: "fetch ready tasks (post-reconcile): " + err.Error(),
					})
					stopReason = stopComputeErrored
					break
				}
			}
		}

		nextCompute, foundBlocker := pickNextComputeCandidate(ready, c.username)
		if nextCompute == nil {
			if foundBlocker != nil {
				// Distinguish citizen-gate from assigned-elsewhere
				// compute so callers can tell "go make a human
				// decision" from "wait on another citizen's
				// machinery" without parsing the blocker field.
				if foundBlocker.Action == "compute" {
					stopReason = stopComputeAssignedElsewhere
				} else {
					stopReason = stopCitizenTaskReady
				}
				blocker = foundBlocker
			} else {
				stopReason = stopNoReadyCompute
			}
			break
		}

		outcome, err := c.executeComputeTask(ctx, nextCompute.taskID)
		if err != nil {
			entries = append(entries, executeRunEntry{
				TaskID: nextCompute.taskID,
				Status: "error",
				Reason: err.Error(),
			})
			stopReason = stopComputeErrored
			break
		}
		entries = append(entries, entryFromOutcome(outcome))

		switch outcome.Status {
		case "failed":
			stopReason = stopComputeFailed
		case "async_started":
			// Async compute spawns a detached subprocess; the
			// task isn't done yet, so continuing would drive
			// downstream into a state where upstream hasn't
			// actually completed. Stop and let the caller come
			// back after the async reap fires.
			stopReason = stopAsyncTaskStarted
		}
		if stopReason != "" {
			break
		}
	}

	if stopReason == "" {
		// Hit the cap — there may still be ready compute tasks.
		stopReason = stopMaxTasks
	}

	return mcp.NewToolResultText(formatExecuteRunSummary(entries, stopReason, blocker, maxTasks)), nil
}

// computeCandidate is the filtered-and-sorted winner from a
// /ready scan. nil means no eligible compute task in this batch
// scan.
type computeCandidate struct {
	taskID string
	seq    int
}

// pickNextComputeCandidate filters the /ready payload and
// picks:
//   - the lowest-seq ready compute task eligible for this
//     citizen (assign_to respected, tasks already claimed by
//     someone else skipped)
//   - AND the lowest-seq ready blocker task to report when no
//     eligible compute is available — preferring citizen
//     actions (vote/review/answer/contribute) over assigned-
//     elsewhere compute, because a human-decision gate is a
//     stronger signal to the operator than "waiting on another
//     citizen's machinery."
//
// Returning both lets the caller's loop prefer computes (drain
// deterministic work first) and only surface the blocker when
// there's nothing auto-advanceable left.
func pickNextComputeCandidate(ready []map[string]interface{}, username string) (*computeCandidate, *executeRunBlocker) {
	type row struct {
		id     string
		action string
		seq    int
		raw    map[string]interface{}
	}
	var computePool []row
	var citizenPool []row
	for _, t := range ready {
		id, _ := t["id"].(string)
		if id == "" {
			continue
		}
		action, _ := t["action"].(string)
		seqF, _ := t["seq"].(float64)
		r := row{id: id, action: action, seq: int(seqF), raw: t}
		if action == "compute" {
			computePool = append(computePool, r)
		} else {
			citizenPool = append(citizenPool, r)
		}
	}
	sort.Slice(computePool, func(i, j int) bool {
		if computePool[i].seq != computePool[j].seq {
			return computePool[i].seq < computePool[j].seq
		}
		return computePool[i].id < computePool[j].id
	})
	sort.Slice(citizenPool, func(i, j int) bool {
		if citizenPool[i].seq != citizenPool[j].seq {
			return citizenPool[i].seq < citizenPool[j].seq
		}
		return citizenPool[i].id < citizenPool[j].id
	})

	// Scan compute pool for the first eligible entry. Track
	// the first ineligible one too — if nothing we can execute
	// is left, that's the blocker we report.
	var pick *computeCandidate
	var ineligibleCompute *executeRunBlocker
	for _, r := range computePool {
		// assign_to: when non-empty, this citizen must be in
		// the list. Skip (RFC decision: assigned compute tasks
		// are treated as blockers, not auto-executed by
		// whoever called execute_run).
		if assigned := stringSliceFromAny(r.raw["assign_to"]); len(assigned) > 0 {
			allowed := false
			for _, u := range assigned {
				if u == username {
					allowed = true
					break
				}
			}
			if !allowed {
				if ineligibleCompute == nil {
					ineligibleCompute = &executeRunBlocker{TaskID: r.id, Action: r.action}
				}
				continue
			}
		}
		// Tasks claimed by another citizen are skipped — the
		// cascade doesn't steal work from someone actively
		// holding a claim.
		if claimedBy, _ := r.raw["claimed_by"].(string); claimedBy != "" && claimedBy != username {
			if ineligibleCompute == nil {
				ineligibleCompute = &executeRunBlocker{TaskID: r.id, Action: r.action}
			}
			continue
		}
		pick = &computeCandidate{taskID: r.id, seq: r.seq}
		break
	}

	// Blocker preference: citizen action > ineligible compute.
	// A ready human gate is always the stronger signal — "go
	// make a decision" beats "wait on another citizen's
	// machinery."
	var blocker *executeRunBlocker
	if len(citizenPool) > 0 {
		blocker = &executeRunBlocker{
			TaskID: citizenPool[0].id,
			Action: citizenPool[0].action,
		}
	} else if ineligibleCompute != nil {
		blocker = ineligibleCompute
	}
	return pick, blocker
}

// fetchRunBranch resolves (project_id, run_id) to the run's
// branch name. Doubles as a pre-flight existence check:
// a nonexistent run surfaces as a clear "run not found" error
// here rather than bleeding through the main loop as a
// misleading "no_ready_compute" (empty /ready). Returns empty
// branch + no error only in the unlikely case where the run
// exists but has no branch field set, which the cascade loop
// handles by skipping the cold-reconcile fallback.
func (c *apiClient) fetchRunBranch(ctx context.Context, projectID, runID int) (string, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID)
	data, err := c.get(ctx, path)
	if err != nil {
		return "", fmt.Errorf("run %d:%d not found: %w", projectID, runID, err)
	}
	var resp map[string]interface{}
	if err := json.Unmarshal(data, &resp); err != nil {
		return "", fmt.Errorf("decoding run response: %w", err)
	}
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		return "", fmt.Errorf("run %d:%d: %s", projectID, runID, errMsg)
	}
	branch, _ := resp["branch"].(string)
	return branch, nil
}

// fetchReadyTasksForRun wraps the /api/v1/tasks/ready endpoint
// scoped to (project, run). Shares the URL shape with
// claim_batch; kept local rather than a generic helper to
// avoid a shared abstraction that would need to evolve for
// non-run-scoped callers later.
func (c *apiClient) fetchReadyTasksForRun(ctx context.Context, projectID, runID int) ([]map[string]interface{}, error) {
	path := fmt.Sprintf("/api/v1/tasks/ready?project_id=%d&run_id=%d", projectID, runID)
	data, err := c.get(ctx, path)
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decoding ready-tasks response: %w", err)
	}
	return out, nil
}

func entryFromOutcome(out *executeOutcome) executeRunEntry {
	e := executeRunEntry{
		TaskID:    out.TaskID,
		Status:    out.Status,
		Script:    out.Script,
		ElapsedMS: out.ElapsedMS,
		CommitSHA: out.CommitSHA,
	}
	switch out.Status {
	case "failed":
		reason := out.Stderr
		if reason == "" {
			reason = out.ErrorMessage
		}
		e.Reason = reason
	case "completed":
		if len(out.ArtifactsWritten) > 0 {
			e.Artifacts = out.ArtifactsWritten
		}
	}
	return e
}

// formatExecuteRunSummary produces the human-readable response.
// Header first (N completed / M failed / stop reason), then one
// line per executed entry. Matches the shape of
// formatClaimMatchingSummary + formatBatchSubmit so a caller
// that parses batch responses can key off the same structure.
func formatExecuteRunSummary(entries []executeRunEntry, stopReason string, blocker *executeRunBlocker, maxTasks int) string {
	var completed, failed, errored, async int
	for _, e := range entries {
		switch e.Status {
		case "completed":
			completed++
		case "failed":
			failed++
		case "async_started":
			async++
		case "error":
			errored++
		}
	}

	var b strings.Builder
	b.WriteString(fmt.Sprintf("Cascade: %d executed", len(entries)))
	if completed > 0 || failed > 0 || async > 0 || errored > 0 {
		parts := []string{}
		if completed > 0 {
			parts = append(parts, fmt.Sprintf("%d completed", completed))
		}
		if failed > 0 {
			parts = append(parts, fmt.Sprintf("%d failed", failed))
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
	case stopCitizenTaskReady:
		if blocker != nil {
			b.WriteString(fmt.Sprintf(" — next_blocker=%s (action=%s)", blocker.TaskID, blocker.Action))
		}
		b.WriteString("\n  → call enju_claim_task for this task; after submitting, call enju_execute_run again to resume.\n")
	case stopComputeAssignedElsewhere:
		if blocker != nil {
			b.WriteString(fmt.Sprintf(" — next_blocker=%s (action=compute, assigned to another citizen)", blocker.TaskID))
		}
		b.WriteString("\n  → wait for the assigned citizen to execute this task, then call enju_execute_run again.\n")
	case stopNoReadyCompute:
		b.WriteString("\n  → run is idle or complete; check enju_run_status.\n")
	case stopComputeFailed:
		b.WriteString("\n  → downstream tasks blocked. Fix the script + enju_invalidate_task, or accept the failure.\n")
	case stopAsyncTaskStarted:
		b.WriteString("\n  → an async compute task is running detached. Call enju_execute_run again once it lands.\n")
	case stopMaxTasks:
		b.WriteString(fmt.Sprintf(" — hit max_tasks=%d; call again to continue.\n", maxTasks))
	case stopContextCancelled:
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

func writeExecuteRunEntryLine(b *strings.Builder, e executeRunEntry) {
	var prefix string
	switch e.Status {
	case "completed":
		prefix = "✓"
	case "failed":
		prefix = "✗"
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
		b.WriteString(fmt.Sprintf(" commit=%s", shortSHA(e.CommitSHA)))
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
