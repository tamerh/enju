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
// Serial by default, parallel via the `parallel` parameter
// (1–8). Parallel mode dispatches up to N compute tasks
// concurrently within a single cascade — scripts run truly in
// parallel, the git layer (commit + push) serializes through
// proj.Lock(), and each completion may unblock new ready tasks
// that get picked up in the next dispatch pass. For typical
// bio workloads where script wall-clock dominates, parallel=N
// gives ~N× speedup with negligible lock contention. See
// runCascadeParallel for the full machinery.

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
	// Parallel-dispatch knob. Default 4 (sweet spot for the
	// typical bio fan-out: 4-way judges, 4-way alignment
	// pipelines), cap at 32 for power users with beefy hosts.
	// Higher values still hit diminishing returns past the
	// proj.Lock contention point on git commit/push, but the
	// cap is permissive — operators who know their workload
	// can opt in. Pass parallel=1 to force serial when
	// debugging or when scripts are RAM-hungry.
	defaultParallel = 4
	maxParallel     = 32
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
	stopComputeFailed            = "compute_failed"
	stopComputeErrored           = "compute_errored"
	// stopGitOperationFailed fires when the script ran fine
	// (exit 0, produced output) but the post-script git
	// commit/push failed. Distinct from compute_failed (script
	// non-zero) and compute_errored (wrapper-level / pre-exec
	// failure) so callers know the work product is on disk and
	// the recovery is fix-the-git-state, not re-run-the-script.
	stopGitOperationFailed = "git_operation_failed"
	stopAsyncTaskStarted   = "async_task_started"
	stopMaxTasks           = "max_tasks"
	stopContextCancelled   = "context_cancelled"
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
	parallel := req.GetInt("parallel", defaultParallel)
	if parallel <= 0 {
		parallel = defaultParallel
	}
	if parallel > maxParallel {
		return mcp.NewToolResultError(fmt.Sprintf(
			"parallel %d exceeds hard cap %d — diminishing returns past the proj.Lock contention point on git commit/push, and bio scripts can be RAM-heavy. If you genuinely need more, split the cascade across multiple enju_execute_run calls.",
			parallel, maxParallel)), nil
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

	// Parallel branch: distinct code path so the proven serial
	// loop below stays untouched. Both share the per-task
	// helpers (executeComputeTask, fetchReadyTasksForRun,
	// entryFromOutcome) and the same stop-reason vocabulary.
	if parallel > 1 {
		entries, stopReason, blocker := c.runCascadeParallel(
			ctx, projectID, runID, runBranch, parallel, maxTasks,
		)
		return mcp.NewToolResultText(formatExecuteRunSummary(entries, stopReason, blocker, maxTasks, parallel)), nil
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
		case "git_failed":
			// Script ran fine; the post-exec commit/push
			// failed. Recovery is fix-the-git-state (e.g.
			// re-add a remote, rebase), not re-run the script —
			// the work product is still on disk under
			// spec.ResultDir. Surfaced as a distinct stop_reason
			// so callers can route to a different recovery path
			// than for compute_failed.
			stopReason = stopGitOperationFailed
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

	return mcp.NewToolResultText(formatExecuteRunSummary(entries, stopReason, blocker, maxTasks, parallel)), nil
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
	case "git_failed":
		// ErrorMessage carries the chained git error
		// ("git submit failed: push failed: ...") so users see
		// the actual git stderr, not just "compute errored."
		e.Reason = out.ErrorMessage
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
func formatExecuteRunSummary(entries []executeRunEntry, stopReason string, blocker *executeRunBlocker, maxTasks, parallel int) string {
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
		if parallel > 1 {
			// In parallel mode, in-flight siblings drain
			// before the cascade returns (complete-then-stop
			// policy: bio scripts may not be safely
			// interruptible mid-run). Surface so operators
			// don't think they're stuck — the cascade IS
			// finishing, just letting in-flight finish first.
			// Ctrl-C still cancels everything via ctx.
			b.WriteString(fmt.Sprintf(" — in-flight siblings drained (parallel=%d, complete-then-stop policy; Ctrl-C cancels via ctx)", parallel))
		}
		b.WriteString("\n  → downstream tasks blocked. Fix the script + enju_invalidate_task, or accept the failure.\n")
	case stopGitOperationFailed:
		// Note: do NOT suggest enju_invalidate_task here. The
		// task is still in `claimed` state (no /result, no /fail
		// reported to the coordinator), so invalidate would
		// reject. executeComputeTask's claim gate already
		// handles the "claimed by us" retry path —
		// re-running enju_execute_run after fixing the remote
		// is sufficient.
		if parallel > 1 {
			b.WriteString(fmt.Sprintf(" — in-flight siblings drained (parallel=%d)", parallel))
		}
		b.WriteString("\n  → script ran fine — work product is still on disk. Failure was at the git layer (commit/push). Inspect enju_project_remote_status, fix the remote state, then call enju_execute_run again to retry.\n")
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
	case "git_failed":
		// Distinct icon so a human reader doesn't confuse it
		// with a script failure — the work product is on disk,
		// just not pushed.
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

// runCascadeParallel is the parallel sibling of the serial loop
// in handleExecuteRun. It dispatches up to `parallel` compute
// tasks concurrently, drains their outcomes as they finish,
// re-fetches the ready set after each completion (so newly
// unblocked downstream tasks get picked up), and stops when:
//
//   - A stop signal fires (compute_failed, git_failed,
//     async_started, citizen_task_ready, compute_assigned_
//     elsewhere, no_ready_compute, max_tasks). Once a stop
//     signal triggers, we stop dispatching new work but let
//     in-flight tasks complete (the "complete-then-stop"
//     policy: bio scripts may not be safely interruptible
//     mid-run, and yanking ctx mid-script could leave
//     half-written artifacts).
//
//   - Context cancellation. Same drain-then-exit policy.
//
// Concurrency model: a `parallel`-bounded semaphore plus a
// channel of results. The git layer (commit + push) serializes
// naturally through proj.Lock() inside executeComputeTask, so
// scripts run truly in parallel and commits queue at the lock.
// For typical bio workloads (script wall-clock dominates),
// this is near-optimal — the lock contention is invisible
// against multi-second script execution.
//
// Why not refactor the serial path to use this same machinery
// with parallel=1? Lower regression risk: the serial path is
// shipped, proven, and has its own tests. The parallel path
// is additive.
//
// Entry ordering: the serial path appends entries in dispatch
// order (lowest seq first) because dispatch and completion are
// the same step. The parallel path appends in COMPLETION order
// (fast scripts before slow ones, regardless of seq). Two runs
// over the same workload can produce different orderings if
// scripts have variable wall-clock. Callers who need a stable
// audit trail should sort by seq downstream; the cascade
// summary is meant for "what happened" not "what dispatched
// when." The git log is the canonical chronological record.
func (c *apiClient) runCascadeParallel(
	ctx context.Context,
	projectID, runID int,
	runBranch string,
	parallel, maxTasks int,
) ([]executeRunEntry, string, *executeRunBlocker) {
	type result struct {
		outcome *executeOutcome
		err     error
		taskID  string
	}

	var entries []executeRunEntry
	var stopReason string
	var blocker *executeRunBlocker

	// Buffered to `parallel` so a worker can hand off its
	// result and exit without waiting for the main loop to
	// receive — useful when the main loop is mid-fetch and
	// can't drain immediately.
	results := make(chan result, parallel)
	inFlight := 0
	// Tracks tasks dispatched in this cascade. Prevents
	// re-dispatch when the same task appears in /ready twice
	// (the coordinator hasn't seen our claim yet during a
	// race between dispatch and the next /ready fetch).
	dispatched := make(map[string]bool)
	coldReconcileTried := false

	// recordEntry appends an outcome and (if it's terminal)
	// sets the stop reason. Idempotent on stopReason: the
	// FIRST stop signal wins, which matches user expectation
	// — a parallel batch where task #1 fails and task #2
	// succeeds reports compute_failed, not "completed."
	recordEntry := func(r result) {
		var e executeRunEntry
		if r.err != nil {
			e = executeRunEntry{
				TaskID: r.taskID,
				Status: "error",
				Reason: r.err.Error(),
			}
		} else {
			e = entryFromOutcome(r.outcome)
		}
		entries = append(entries, e)
		if stopReason != "" {
			return // first signal wins
		}
		switch e.Status {
		case "failed":
			stopReason = stopComputeFailed
		case "git_failed":
			stopReason = stopGitOperationFailed
		case "async_started":
			stopReason = stopAsyncTaskStarted
		case "error":
			stopReason = stopComputeErrored
		}
	}

	// drainNonBlocking pulls every completed result off the
	// channel without waiting. Called before each dispatch
	// pass so the in-flight count reflects current reality.
	drainNonBlocking := func() {
		for {
			select {
			case r := <-results:
				inFlight--
				recordEntry(r)
			default:
				return
			}
		}
	}

	// waitOne blocks until at least one worker finishes.
	// Used both during normal flow (between dispatch passes)
	// and during shutdown (after a stop signal, to drain
	// in-flight tasks before exiting).
	waitOne := func() {
		r := <-results
		inFlight--
		recordEntry(r)
	}

	// Drain helper for the shutdown path. Loops waitOne
	// until in-flight is zero.
	drainAll := func() {
		for inFlight > 0 {
			waitOne()
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			if stopReason == "" {
				stopReason = stopContextCancelled
			}
			drainAll()
			break
		}

		drainNonBlocking()

		// Stop signal latched? Drain in-flight then exit.
		if stopReason != "" {
			drainAll()
			break
		}

		// Fetch ready set for the next dispatch pass.
		ready, err := c.fetchReadyTasksForRun(ctx, projectID, runID)
		if err != nil {
			entries = append(entries, executeRunEntry{
				Status: "error",
				Reason: "fetch ready tasks: " + err.Error(),
			})
			stopReason = stopComputeErrored
			drainAll()
			break
		}

		// Cold-start reconcile (same flow as the serial
		// path): if nothing's been done yet AND nothing's in
		// flight AND /ready is empty AND we haven't tried
		// reconciling, do one pull to disambiguate "run is
		// idle" from "stale post-async-completion state."
		if !coldReconcileTried && inFlight == 0 && len(entries) == 0 && len(ready) == 0 && runBranch != "" {
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
					drainAll()
					break
				}
			}
		}

		// Pick all eligible candidates (vs. the serial
		// path's "lowest-seq one"). The candidates are
		// already filtered by assign_to + claimed_by + the
		// in-cascade `dispatched` set.
		candidates, foundBlocker := pickAllEligibleCompute(ready, c.username, dispatched)

		// Dispatch up to capacity. Cap at parallel and at
		// max_tasks budget (entries-already-recorded plus
		// in-flight already counts toward the budget).
		newDispatched := 0
		for _, cand := range candidates {
			if inFlight >= parallel {
				break
			}
			if len(entries)+inFlight >= maxTasks {
				break
			}
			dispatched[cand.taskID] = true
			inFlight++
			newDispatched++
			go func(taskID string) {
				outcome, err := c.executeComputeTask(ctx, taskID)
				results <- result{outcome: outcome, err: err, taskID: taskID}
			}(cand.taskID)
		}

		// Terminal state check: nothing dispatched, nothing
		// in flight. Decide why and exit. The four cases:
		//
		//   1. We had eligible candidates but the max_tasks
		//      budget is exhausted → stop with max_tasks.
		//      (Without this branch, the loop would mis-
		//      report no_ready_compute when there's still
		//      work the caller could pick up next call.)
		//   2. A blocker (citizen action / assigned-else-
		//      where compute) is the only ready thing.
		//   3. No blocker, no candidates, no in-flight →
		//      run is idle or complete.
		if newDispatched == 0 && inFlight == 0 {
			if len(candidates) > 0 {
				// Had work but the budget said "stop." This
				// is the deterministic path into stopMaxTasks
				// when the parallel loop runs into the cap.
				stopReason = stopMaxTasks
			} else if foundBlocker != nil {
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

		// Wait for at least one to finish before the next
		// dispatch pass. This is the "drain then dispatch
		// then wait" rhythm that keeps the in-flight count
		// honest and gives newly-unblocked downstream tasks
		// a chance to enter the ready set.
		if inFlight > 0 {
			waitOne()
		}
	}

	if stopReason == "" {
		// Hit max_tasks budget and drained.
		stopReason = stopMaxTasks
	}
	return entries, stopReason, blocker
}

// pickAllEligibleCompute is the parallel-mode counterpart to
// pickNextComputeCandidate. Returns ALL eligible compute tasks
// (lowest-seq first) instead of just one, so the caller can
// dispatch up to its parallel budget in a single pass. Skips
// tasks already dispatched in this cascade (the `dispatched`
// set) so a re-fetch that still shows our task as ready
// doesn't double-dispatch.
//
// Blocker semantics match the serial helper: when no eligible
// compute is available, return the lowest-seq citizen task as
// the human-decision blocker, falling back to ineligible
// compute (assigned-elsewhere) if no citizen action is ready.
func pickAllEligibleCompute(ready []map[string]interface{}, username string, dispatched map[string]bool) ([]computeCandidate, *executeRunBlocker) {
	type row struct {
		id     string
		action string
		seq    int
		raw    map[string]interface{}
	}
	var computePool, citizenPool []row
	for _, t := range ready {
		id, _ := t["id"].(string)
		if id == "" {
			continue
		}
		if dispatched[id] {
			// Already running in this cascade — don't redispatch.
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

	var picks []computeCandidate
	var ineligibleCompute *executeRunBlocker
	for _, r := range computePool {
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
		if claimedBy, _ := r.raw["claimed_by"].(string); claimedBy != "" && claimedBy != username {
			if ineligibleCompute == nil {
				ineligibleCompute = &executeRunBlocker{TaskID: r.id, Action: r.action}
			}
			continue
		}
		picks = append(picks, computeCandidate{taskID: r.id, seq: r.seq})
	}

	// If we have any eligible compute, blocker is irrelevant
	// — the caller will dispatch and look again next pass.
	if len(picks) > 0 {
		return picks, nil
	}
	var blocker *executeRunBlocker
	if len(citizenPool) > 0 {
		blocker = &executeRunBlocker{TaskID: citizenPool[0].id, Action: citizenPool[0].action}
	} else if ineligibleCompute != nil {
		blocker = ineligibleCompute
	}
	return picks, blocker
}
