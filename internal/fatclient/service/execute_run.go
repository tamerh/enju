package service

// Cascade execution. FatClient.ExecuteRun drains all auto-
// advanceable compute tasks in a run; citizen tasks (vote,
// review, answer, contribute) are never auto-advanced and the
// cascade stops at the next human gate, reporting it as
// `Blocker` so the operator knows where the pipeline paused.
//
// Two code paths share the per-task helpers (ExecuteComputeTask,
// fetchReadyTasksForRun) and the same stop-reason vocabulary:
//
//   - Serial loop (parallel == 1): proven shape, one task at a
//     time, simple ordering.
//   - Parallel dispatch (parallel > 1): up to N compute tasks
//     concurrently, complete-then-stop on a stop signal so
//     in-flight bio scripts aren't yanked mid-run.
//
// Handlers translate the structured (entries, stopReason,
// blocker) result to MCP text.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/enju-ai/enju/internal/common/format"
	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// ExecuteRunEntry captures one task's outcome in the batch
// cascade. Separate from ExecuteOutcome because the batch
// response wants a lean per-entry line without the full
// structured payload each step emits.
type ExecuteRunEntry struct {
	TaskID    string   `json:"task_id"`
	Status    string   `json:"status"`
	Script    string   `json:"script,omitempty"`
	ElapsedMS int64    `json:"elapsed_ms,omitempty"`
	CommitSHA string   `json:"commit_sha,omitempty"`
	Reason    string   `json:"reason,omitempty"`
	Artifacts []string `json:"artifacts,omitempty"`
}

// ExecuteRunBlocker names the lowest-seq ready task that the
// cascade could not auto-advance — either a citizen action or
// a compute task assigned elsewhere.
type ExecuteRunBlocker struct {
	TaskID string `json:"task_id"`
	Action string `json:"action"`
}

// Stop reasons. Surfaced verbatim to callers (and on through
// to handler formatting) so they can script behavior per
// reason without substring-matching free text.
const (
	StopNoReadyCompute           = "no_ready_compute"
	StopCitizenTaskReady         = "citizen_task_ready"
	StopComputeAssignedElsewhere = "compute_assigned_elsewhere"
	StopComputeFailed            = "compute_failed"
	StopComputeErrored           = "compute_errored"
	// StopGitOperationFailed fires when the script ran fine
	// (exit 0, produced output) but the post-script git
	// commit/push failed. Distinct from compute_failed (script
	// non-zero) and compute_errored (wrapper-level / pre-exec
	// failure) so callers know the work product is on disk and
	// the recovery is fix-the-git-state, not re-run-the-script.
	StopGitOperationFailed = "git_operation_failed"
	StopAsyncTaskStarted   = "async_task_started"
	StopMaxTasks           = "max_tasks"
	StopContextCancelled   = "context_cancelled"
)

// stopReasonForOutcome maps a per-task outcome status to the stop
// reason that should END the cascade, or "" to keep draining. The one
// place both the serial loop and runCascadeParallel's recordEntry
// consult, so the keep-going policy can't drift between them.
//
//   - failed / git_failed  → task-level, recoverable. Fatal by default;
//     under keepGoing, "" (record it, keep going — the task parks
//     failed_retryable and drops out of the next /ready scan).
//   - error                → driver-level (claim/fetch/wrapper pre-exec).
//     Always fatal: no progress is possible, keepGoing notwithstanding.
//   - async_started        → a detached launch; the pass stops so the
//     caller can reap (a drive loop treats this as "relaunch later").
//   - anything else (completed/skipped) → "".
func stopReasonForOutcome(status string, keepGoing bool) string {
	switch status {
	case "failed":
		if keepGoing {
			return ""
		}
		return StopComputeFailed
	case "git_failed":
		if keepGoing {
			return ""
		}
		return StopGitOperationFailed
	case "error":
		return StopComputeErrored
	case "async_started":
		return StopAsyncTaskStarted
	default:
		return ""
	}
}

// MaxParallel is the hard ceiling on ExecuteRunParams.Parallel,
// shared by every front door to ExecuteRun (the MCP enju_execute_run
// handler and the `enju go --parallel` CLI flag) so the bound is
// structural, not duplicated per caller. Past this point the
// project-lock contention on git commit/push dominates and compute
// scripts can be RAM-heavy.
const MaxParallel = 32

// ExecuteRunParams is the input for FatClient.ExecuteRun.
//
// RunSeq is the run's PER-PROJECT sequence number (the "#11" a
// user sees), NOT the global runs.id. Every coord run endpoint
// resolves /projects/{pid}/runs/{X} as project-seq, so callers
// MUST pass the seq. The field was historically named RunID,
// which invited `enju go` to thread the global id and produced
// the silent "run P:ID not found" failure for every CLI run.
// The rename is the by-construction guard: a caller writing
// `RunID:` no longer compiles.
type ExecuteRunParams struct {
	ProjectID int
	RunSeq    int
	MaxTasks  int
	Parallel  int
	// KeepGoing, when set, makes a task-level failure (compute_failed
	// / git_failed) NON-fatal to the cascade: the failed entry is
	// recorded but the loop keeps draining other ready tasks instead
	// of stopping. The failed task parks failed_retryable and its
	// descendants block by ordinary dependency-not-satisfied, so
	// independent branches (e.g. sibling genes in a fan-out) still
	// complete. Driver-level errors (compute_errored, context
	// cancellation) and async launches still stop the pass regardless.
	// Default false = stop on the first task-level failure (fail-fast).
	KeepGoing bool
}

// ExecuteRunResult bundles the cascade result.
type ExecuteRunResult struct {
	Entries    []ExecuteRunEntry
	StopReason string
	Blocker    *ExecuteRunBlocker
	// SelfStuckClaims lists task IDs in this run currently
	// held by the calling citizen in claimed/running state —
	// the most common cause of a "no_ready_compute" stop after
	// an interrupted prior execute_run. Populated only when
	// StopReason == StopNoReadyCompute, so handlers can render
	// a self-recovery hint ("call enju_release_task X then
	// retry") instead of the generic "run is idle" message.
	// Empty in healthy completions.
	SelfStuckClaims []string
}

// ExecuteRun drains ready compute tasks for (project, run)
// until a stop condition fires. Both serial and parallel
// modes share the same per-task ExecuteComputeTask path so a
// single fix to the worker propagates to both.
func (s *FatClient) ExecuteRun(ctx context.Context, p ExecuteRunParams) (*ExecuteRunResult, error) {
	if s.enjugit == nil {
		return nil, fmt.Errorf("enju_execute_run requires a local workspace")
	}

	// Pre-flight: resolve the run. Gives a real "run not found"
	// error (vs. the misleading "idle run" the main loop would
	// produce for an empty /ready on a nonexistent run) and
	// caches the branch for the cold-reconcile fallback below.
	runBranch, runState, err := s.fetchRunBranch(ctx, p.ProjectID, p.RunSeq)
	if err != nil {
		return nil, err
	}
	// B-2: refuse to drive a PAUSED run. PAUSED is the operator's
	// circuit-breaker ("stop progress so I can inspect"); the
	// coord-side claim/submit gating that would enforce this is
	// an acknowledged deferred gap (see enju_pause_run docs), so
	// without this guard enju_execute_run blew straight through a
	// pause and drove every task to ACCEPTED — then the run sat
	// stuck at "paused 100%" until a manual resume re-evaluated
	// it. Fail closed with an actionable error instead; the
	// operator resumes deliberately when they're ready.
	if runState == "paused" {
		return nil, fmt.Errorf(
			"run %d:%d is paused — enju_execute_run will not advance a paused run "+
				"(pause is a circuit-breaker). Resume it first with "+
				"enju_resume_run(project_id=%d, run_id=%d), then re-run.",
			p.ProjectID, p.RunSeq, p.ProjectID, p.RunSeq)
	}

	if p.Parallel > 1 {
		entries, stopReason, blocker := s.runCascadeParallel(
			ctx, p.ProjectID, p.RunSeq, runBranch, p.Parallel, p.MaxTasks, p.KeepGoing,
		)
		res := &ExecuteRunResult{Entries: entries, StopReason: stopReason, Blocker: blocker}
		if stopReason == StopNoReadyCompute {
			res.SelfStuckClaims = s.findSelfHeldStuckTasks(ctx, p.ProjectID, p.RunSeq)
		}
		return res, nil
	}

	var entries []ExecuteRunEntry
	var stopReason string
	var blocker *ExecuteRunBlocker
	coldReconcileTried := false

	for len(entries) < p.MaxTasks {
		if err := ctx.Err(); err != nil {
			stopReason = StopContextCancelled
			break
		}

		ready, err := s.fetchReadyTasksForRun(ctx, p.ProjectID, p.RunSeq)
		if err != nil {
			entries = append(entries, ExecuteRunEntry{
				Status: "error",
				Reason: "fetch ready tasks: " + err.Error(),
			})
			stopReason = StopComputeErrored
			break
		}

		// Cold-start reconcile. If we're called without a recent
		// submit in this session (e.g. an overnight async job
		// just landed), an empty /ready on the first scan is
		// ambiguous between "nothing to do" and "stale state" —
		// do a pull-and-reconcile once to disambiguate before
		// concluding the run is idle.
		if !coldReconcileTried && len(entries) == 0 && len(ready) == 0 && runBranch != "" {
			coldReconcileTried = true
			if wf, _, _, _, perr := s.OpenWorkflow(ctx, int64(p.ProjectID)); perr == nil && wf != nil {
				// No-checkout reconcile: the cascade is compute-only,
				// so don't move the operator's worktree onto the run
				// branch (see reconcileBranchWF).
				s.reconcileBranchWF(ctx, wf, int64(p.ProjectID), runBranch)
				ready, err = s.fetchReadyTasksForRun(ctx, p.ProjectID, p.RunSeq)
				if err != nil {
					entries = append(entries, ExecuteRunEntry{
						Status: "error",
						Reason: "fetch ready tasks (post-reconcile): " + err.Error(),
					})
					stopReason = StopComputeErrored
					break
				}
			}
		}

		nextCompute, foundBlocker := pickNextComputeCandidate(ready, s.coord.Username())
		if nextCompute == nil {
			if foundBlocker != nil {
				if foundBlocker.Action == "compute" {
					stopReason = StopComputeAssignedElsewhere
				} else {
					stopReason = StopCitizenTaskReady
				}
				blocker = foundBlocker
			} else {
				stopReason = StopNoReadyCompute
			}
			break
		}

		outcome, err := s.ExecuteComputeTask(ctx, nextCompute.TaskID)
		if err != nil {
			entries = append(entries, ExecuteRunEntry{
				TaskID: nextCompute.TaskID,
				Status: "error",
				Reason: err.Error(),
			})
			stopReason = StopComputeErrored
			break
		}
		entries = append(entries, EntryFromOutcome(outcome))

		if r := stopReasonForOutcome(outcome.Status, p.KeepGoing); r != "" {
			stopReason = r
			break
		}
	}

	if stopReason == "" {
		stopReason = StopMaxTasks
	}

	res := &ExecuteRunResult{Entries: entries, StopReason: stopReason, Blocker: blocker}
	if stopReason == StopNoReadyCompute {
		res.SelfStuckClaims = s.findSelfHeldStuckTasks(ctx, p.ProjectID, p.RunSeq)
	}
	return res, nil
}

// ComputeCandidate is the filtered-and-sorted winner from a
// /ready scan.
type ComputeCandidate struct {
	TaskID string
	Seq    int
}

// pickNextComputeCandidate filters the /ready payload and
// picks the lowest-seq ready compute task eligible for this
// citizen, plus the lowest-seq ready blocker task to report
// when no eligible compute is available — preferring citizen
// actions over assigned-elsewhere compute, because a human-
// decision gate is a stronger signal to the operator.
func pickNextComputeCandidate(ready []map[string]interface{}, username string) (*ComputeCandidate, *ExecuteRunBlocker) {
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

	var pick *ComputeCandidate
	var ineligibleCompute *ExecuteRunBlocker
	for _, r := range computePool {
		if assigned := format.StringSliceFromAny(r.raw["assign_to"]); len(assigned) > 0 {
			allowed := false
			for _, u := range assigned {
				if u == username {
					allowed = true
					break
				}
			}
			if !allowed {
				if ineligibleCompute == nil {
					ineligibleCompute = &ExecuteRunBlocker{TaskID: r.id, Action: r.action}
				}
				continue
			}
		}
		if claimedBy, _ := r.raw["claimed_by"].(string); claimedBy != "" && claimedBy != username {
			if ineligibleCompute == nil {
				ineligibleCompute = &ExecuteRunBlocker{TaskID: r.id, Action: r.action}
			}
			continue
		}
		pick = &ComputeCandidate{TaskID: r.id, Seq: r.seq}
		break
	}

	var blocker *ExecuteRunBlocker
	if len(citizenPool) > 0 {
		blocker = &ExecuteRunBlocker{
			TaskID: citizenPool[0].id,
			Action: citizenPool[0].action,
		}
	} else if ineligibleCompute != nil {
		blocker = ineligibleCompute
	}
	return pick, blocker
}

// pickAllEligibleCompute is the parallel-mode counterpart to
// pickNextComputeCandidate. Returns ALL eligible compute tasks
// (lowest-seq first) so the caller can dispatch up to its
// parallel budget in a single pass. Skips tasks already
// dispatched in this cascade so a re-fetch that still shows
// our task as ready doesn't double-dispatch.
func pickAllEligibleCompute(ready []map[string]interface{}, username string, dispatched map[string]bool) ([]ComputeCandidate, *ExecuteRunBlocker) {
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

	var picks []ComputeCandidate
	var ineligibleCompute *ExecuteRunBlocker
	for _, r := range computePool {
		if assigned := format.StringSliceFromAny(r.raw["assign_to"]); len(assigned) > 0 {
			allowed := false
			for _, u := range assigned {
				if u == username {
					allowed = true
					break
				}
			}
			if !allowed {
				if ineligibleCompute == nil {
					ineligibleCompute = &ExecuteRunBlocker{TaskID: r.id, Action: r.action}
				}
				continue
			}
		}
		if claimedBy, _ := r.raw["claimed_by"].(string); claimedBy != "" && claimedBy != username {
			if ineligibleCompute == nil {
				ineligibleCompute = &ExecuteRunBlocker{TaskID: r.id, Action: r.action}
			}
			continue
		}
		picks = append(picks, ComputeCandidate{TaskID: r.id, Seq: r.seq})
	}

	if len(picks) > 0 {
		return picks, nil
	}
	var blocker *ExecuteRunBlocker
	if len(citizenPool) > 0 {
		blocker = &ExecuteRunBlocker{TaskID: citizenPool[0].id, Action: citizenPool[0].action}
	} else if ineligibleCompute != nil {
		blocker = ineligibleCompute
	}
	return picks, blocker
}

// fetchRunBranch resolves (project_id, run_id) to the run's
// branch name AND state. Doubles as a pre-flight existence
// check: a nonexistent run surfaces as a clear "run not found"
// error here rather than bleeding through the main loop as a
// misleading "no_ready_compute" (empty /ready). The state lets
// ExecuteRun refuse to drive a PAUSED run (bug hunt B-2).
func (s *FatClient) fetchRunBranch(ctx context.Context, projectID, runSeq int) (branch, state string, err error) {
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runSeq)
	data, gerr := s.coord.Get(ctx, path)
	if gerr != nil {
		return "", "", fmt.Errorf("run %d:%d not found: %w", projectID, runSeq, gerr)
	}
	var resp map[string]interface{}
	if uerr := json.Unmarshal(data, &resp); uerr != nil {
		return "", "", fmt.Errorf("decoding run response: %w", uerr)
	}
	if errMsg, _ := resp["error"].(string); errMsg != "" {
		return "", "", fmt.Errorf("run %d:%d: %s", projectID, runSeq, errMsg)
	}
	branch, _ = resp["branch"].(string)
	state, _ = resp["state"].(string)
	return branch, state, nil
}

// ListReadyTasks returns the READY tasks for a (project, run)
// pair. Same wire shape the coord-side `enju_list_ready_tasks`
// MCP tool returns, surfaced as a typed FatClient method so
// in-process consumers (bot daemon, future UI surfaces) don't
// need the raw coord escape hatch.
//
// runID == 0 fetches across every run in the project; non-zero
// scopes to that run. The legacy fetchReadyTasksForRun helper
// below now delegates here.
func (s *FatClient) ListReadyTasks(ctx context.Context, projectID, runID int64) ([]map[string]interface{}, error) {
	path := fmt.Sprintf("/api/v1/tasks/ready?project_id=%d", projectID)
	if runID > 0 {
		path += fmt.Sprintf("&run_id=%d", runID)
	}
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decoding ready-tasks response: %w", err)
	}
	return out, nil
}

// findSelfHeldStuckTasks lists task IDs in this run that the
// calling citizen currently holds an open claim on while the
// task is in claimed/running state — the canonical signature
// of an interrupted prior execute_run. The most common path:
// the operator ESC'd a previous call, the wrap-task subprocess
// (which is detached via Setsid) either finished without writing
// its result file or got killed before it could; the claim row
// stays open until the reaper expires it 30 minutes after the
// claim deadline. In the meantime the cascade routes around the
// orphan and reports no_ready_compute, with no hint that a
// single enju_release_task call would unblock progress.
//
// Best-effort: any error fetching the run's task list is
// swallowed and treated as "no stuck claims found." The caller
// has already decided the run is idle; getting this lookup
// wrong shouldn't escalate to a hard failure.
func (s *FatClient) findSelfHeldStuckTasks(ctx context.Context, projectID, runSeq int) []string {
	username := s.coord.Username()
	if username == "" {
		return nil
	}
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", projectID, runSeq)
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil
	}
	var tasks []map[string]interface{}
	if err := json.Unmarshal(data, &tasks); err != nil {
		return nil
	}
	var stuck []string
	for _, t := range tasks {
		state, _ := t["state"].(string)
		if state != "claimed" && state != "running" {
			continue
		}
		holder, _ := t["claimed_by"].(string)
		if holder != username {
			continue
		}
		id, _ := t["id"].(string)
		if id != "" {
			stuck = append(stuck, id)
		}
	}
	return stuck
}

// CountRunningTasks returns how many tasks in the run are in RUNNING
// state — i.e. async compute subprocesses still in flight (a detached
// `enju wrap-task` whose result the reaper hasn't picked up yet, or one
// still executing). The `enju drive` loop uses this to tell "the run is
// still cooking, keep waiting+reaping" apart from "nothing ready and
// nothing in flight" (a genuine stall it should report rather than spin
// on). Best-effort: any fetch/decode error returns (0, err) and the
// caller treats it conservatively.
func (s *FatClient) CountRunningTasks(ctx context.Context, projectID, runSeq int) (int, error) {
	data, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d/runs/%d/tasks", projectID, runSeq))
	if err != nil {
		return 0, err
	}
	var tasks []map[string]interface{}
	if err := json.Unmarshal(data, &tasks); err != nil {
		return 0, err
	}
	n := 0
	for _, t := range tasks {
		if state, _ := t["state"].(string); state == "running" {
			n++
		}
	}
	return n, nil
}

// fetchReadyTasksForRun wraps the /api/v1/tasks/ready endpoint
// scoped to (project, run).
func (s *FatClient) fetchReadyTasksForRun(ctx context.Context, projectID, runSeq int) ([]map[string]interface{}, error) {
	path := fmt.Sprintf("/api/v1/tasks/ready?project_id=%d&run_id=%d", projectID, runSeq)
	data, err := s.coord.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	var out []map[string]interface{}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decoding ready-tasks response: %w", err)
	}
	return out, nil
}

// EntryFromOutcome adapts an ExecuteOutcome to the lean
// ExecuteRunEntry shape the cascade response uses.
func EntryFromOutcome(out *ExecuteOutcome) ExecuteRunEntry {
	e := ExecuteRunEntry{
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
		e.Reason = out.ErrorMessage
	case "completed":
		if len(out.ArtifactsWritten) > 0 {
			e.Artifacts = out.ArtifactsWritten
		}
	}
	return e
}

// EntryClass is the presentation-neutral classification of an
// ExecuteRunEntry.Status. It exists so every renderer (CLI, MCP,
// …) derives success/failure from ONE place instead of each
// carrying its own `switch e.Status`. Those switches drifted
// once: the CLI renderer only knew "ok"/"accepted" as success,
// so the canonical success status "completed" fell to its ✗
// default arm and every successful `enju go` task printed as a
// failure with an empty reason. Adding a new Status string is
// now a single edit here; renderers switch on the closed
// EntryClass set and keep working. Glyph choice stays in each
// renderer — this only unifies the string→meaning map that was
// duplicated, not the presentation.
type EntryClass int

const (
	// EntryClassUnknown is the fail-safe default: an
	// unrecognized status must never read as success, so
	// renderers treat Unknown as a failure-ish ✗/!, never ✓.
	EntryClassUnknown EntryClass = iota
	EntryClassSuccess            // "completed"
	EntryClassFailed             // "failed" (task script non-zero)
	EntryClassGitFailed          // "git_failed" (commit/push failed)
	EntryClassError              // "error" (coord/claim/infra)
	EntryClassPending            // "async_started" (not terminal)
	EntryClassSkipped            // "skipped"
)

// ClassifyEntryStatus maps an ExecuteRunEntry.Status to its
// EntryClass. The recognized strings are exactly those producers
// in this package set — see EntryFromOutcome ("completed",
// "failed", "git_failed"), execute.go ("async_started"), and the
// Status:"error" sites in ExecuteRun/runCascadeParallel. Anything
// else is EntryClassUnknown by design.
func ClassifyEntryStatus(status string) EntryClass {
	switch status {
	case "completed":
		return EntryClassSuccess
	case "failed":
		return EntryClassFailed
	case "git_failed":
		return EntryClassGitFailed
	case "error":
		return EntryClassError
	case "async_started":
		return EntryClassPending
	case "skipped":
		return EntryClassSkipped
	default:
		return EntryClassUnknown
	}
}

// runCascadeParallel is the parallel sibling of the serial
// loop in ExecuteRun. Dispatches up to `parallel` compute
// tasks concurrently, drains their outcomes as they finish,
// re-fetches the ready set after each completion, and stops
// when a stop signal fires or the context cancels. Once a
// stop signal triggers, no new work is dispatched but in-
// flight tasks complete (the "complete-then-stop" policy:
// bio scripts may not be safely interruptible mid-run).
//
// Concurrency model: a `parallel`-bounded semaphore plus a
// channel of results. The git layer (commit + push) serializes
// naturally through proj.Lock() inside ExecuteComputeTask, so
// scripts run truly in parallel and commits queue at the lock.
//
// Entry ordering: appends in COMPLETION order (fast scripts
// before slow ones, regardless of seq). Two runs over the
// same workload can produce different orderings if scripts
// have variable wall-clock — callers who need a stable audit
// trail should sort by seq downstream. The git log is the
// canonical chronological record.
func (s *FatClient) runCascadeParallel(
	ctx context.Context,
	projectID, runSeq int,
	runBranch string,
	parallel, maxTasks int,
	keepGoing bool,
) ([]ExecuteRunEntry, string, *ExecuteRunBlocker) {
	type result struct {
		outcome *ExecuteOutcome
		err     error
		taskID  string
	}

	var entries []ExecuteRunEntry
	var stopReason string
	var blocker *ExecuteRunBlocker

	results := make(chan result, parallel)
	inFlight := 0
	dispatched := make(map[string]bool)
	coldReconcileTried := false

	// recordEntry appends an outcome and (if it's terminal)
	// sets the stop reason. Idempotent on stopReason: the
	// FIRST stop signal wins, which matches user expectation.
	recordEntry := func(r result) {
		var e ExecuteRunEntry
		if r.err != nil {
			e = ExecuteRunEntry{
				TaskID: r.taskID,
				Status: "error",
				Reason: r.err.Error(),
			}
		} else {
			e = EntryFromOutcome(r.outcome)
		}
		entries = append(entries, e)
		if stopReason != "" {
			return
		}
		stopReason = stopReasonForOutcome(e.Status, keepGoing)
	}

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

	waitOne := func() {
		r := <-results
		inFlight--
		recordEntry(r)
	}

	drainAll := func() {
		for inFlight > 0 {
			waitOne()
		}
	}

	for {
		if err := ctx.Err(); err != nil {
			if stopReason == "" {
				stopReason = StopContextCancelled
			}
			drainAll()
			break
		}

		drainNonBlocking()

		if stopReason != "" {
			drainAll()
			break
		}

		ready, err := s.fetchReadyTasksForRun(ctx, projectID, runSeq)
		if err != nil {
			entries = append(entries, ExecuteRunEntry{
				Status: "error",
				Reason: "fetch ready tasks: " + err.Error(),
			})
			stopReason = StopComputeErrored
			drainAll()
			break
		}

		if !coldReconcileTried && inFlight == 0 && len(entries) == 0 && len(ready) == 0 && runBranch != "" {
			coldReconcileTried = true
			if wf, _, _, _, perr := s.OpenWorkflow(ctx, int64(projectID)); perr == nil && wf != nil {
				// No-checkout reconcile (compute-only cascade).
				s.reconcileBranchWF(ctx, wf, int64(projectID), runBranch)
				ready, err = s.fetchReadyTasksForRun(ctx, projectID, runSeq)
				if err != nil {
					entries = append(entries, ExecuteRunEntry{
						Status: "error",
						Reason: "fetch ready tasks (post-reconcile): " + err.Error(),
					})
					stopReason = StopComputeErrored
					drainAll()
					break
				}
			}
		}

		candidates, foundBlocker := pickAllEligibleCompute(ready, s.coord.Username(), dispatched)

		newDispatched := 0
		for _, cand := range candidates {
			if inFlight >= parallel {
				break
			}
			if len(entries)+inFlight >= maxTasks {
				break
			}
			dispatched[cand.TaskID] = true
			inFlight++
			newDispatched++
			go func(taskID string) {
				outcome, err := s.ExecuteComputeTask(ctx, taskID)
				results <- result{outcome: outcome, err: err, taskID: taskID}
			}(cand.TaskID)
		}

		// Terminal state check: nothing dispatched, nothing in
		// flight. Decide why and exit.
		if newDispatched == 0 && inFlight == 0 {
			if len(candidates) > 0 {
				stopReason = StopMaxTasks
			} else if foundBlocker != nil {
				if foundBlocker.Action == "compute" {
					stopReason = StopComputeAssignedElsewhere
				} else {
					stopReason = StopCitizenTaskReady
				}
				blocker = foundBlocker
			} else {
				stopReason = StopNoReadyCompute
			}
			break
		}

		if inFlight > 0 {
			waitOne()
		}
	}

	if stopReason == "" {
		stopReason = StopMaxTasks
	}
	return entries, stopReason, blocker
}
