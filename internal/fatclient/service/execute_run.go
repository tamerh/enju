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

// ExecuteRunParams is the input for FatClient.ExecuteRun.
type ExecuteRunParams struct {
	ProjectID int
	RunID     int
	MaxTasks  int
	Parallel  int
}

// ExecuteRunResult bundles the cascade result.
type ExecuteRunResult struct {
	Entries    []ExecuteRunEntry
	StopReason string
	Blocker    *ExecuteRunBlocker
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
	runBranch, err := s.fetchRunBranch(ctx, p.ProjectID, p.RunID)
	if err != nil {
		return nil, err
	}

	if p.Parallel > 1 {
		entries, stopReason, blocker := s.runCascadeParallel(
			ctx, p.ProjectID, p.RunID, runBranch, p.Parallel, p.MaxTasks,
		)
		return &ExecuteRunResult{Entries: entries, StopReason: stopReason, Blocker: blocker}, nil
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

		ready, err := s.fetchReadyTasksForRun(ctx, p.ProjectID, p.RunID)
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
				_ = s.PullBranchWithReconcileWF(ctx, wf, int64(p.ProjectID), runBranch)
				ready, err = s.fetchReadyTasksForRun(ctx, p.ProjectID, p.RunID)
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

		switch outcome.Status {
		case "failed":
			stopReason = StopComputeFailed
		case "git_failed":
			stopReason = StopGitOperationFailed
		case "async_started":
			stopReason = StopAsyncTaskStarted
		}
		if stopReason != "" {
			break
		}
	}

	if stopReason == "" {
		stopReason = StopMaxTasks
	}

	return &ExecuteRunResult{Entries: entries, StopReason: stopReason, Blocker: blocker}, nil
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
// branch name. Doubles as a pre-flight existence check:
// a nonexistent run surfaces as a clear "run not found" error
// here rather than bleeding through the main loop as a
// misleading "no_ready_compute" (empty /ready).
func (s *FatClient) fetchRunBranch(ctx context.Context, projectID, runID int) (string, error) {
	path := fmt.Sprintf("/api/v1/projects/%d/runs/%d", projectID, runID)
	data, err := s.coord.Get(ctx, path)
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

// fetchReadyTasksForRun wraps the /api/v1/tasks/ready endpoint
// scoped to (project, run).
func (s *FatClient) fetchReadyTasksForRun(ctx context.Context, projectID, runID int) ([]map[string]interface{}, error) {
	path := fmt.Sprintf("/api/v1/tasks/ready?project_id=%d&run_id=%d", projectID, runID)
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
	projectID, runID int,
	runBranch string,
	parallel, maxTasks int,
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
		switch e.Status {
		case "failed":
			stopReason = StopComputeFailed
		case "git_failed":
			stopReason = StopGitOperationFailed
		case "async_started":
			stopReason = StopAsyncTaskStarted
		case "error":
			stopReason = StopComputeErrored
		}
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

		ready, err := s.fetchReadyTasksForRun(ctx, projectID, runID)
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
				_ = s.PullBranchWithReconcileWF(ctx, wf, int64(projectID), runBranch)
				ready, err = s.fetchReadyTasksForRun(ctx, projectID, runID)
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
