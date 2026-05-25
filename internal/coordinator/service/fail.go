package service

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// FailCascadeResult summarizes the outcome of PerformFailCascade
// for logging + response rendering. Mirrors InvalidationResult
// but with terminal semantics — the target is FAILED (not back
// to READY) and intra-run descendants are SKIPPED (not PENDING).
type FailCascadeResult struct {
	Task        *store.TaskRecord
	Reason       string
	SkippedDescendants []string
	Dematerialized   []string
	Changed      int
	Rollbacks     []RollbackOutcome
}

// PerformFailCascade is the reject/fail analogue of
// PerformInvalidate. Used when a writer task terminates
// unsuccessfully (review reject, enju_fail_task, compute error)
// and downstream consumers must be told the data isn't coming.
//
// Semantic contract (see docs/rollback.md § Rejection vs invalidation):
//
//  1. Target → FAILED with the supplied reason. Terminal —
//   unlike request_changes which bounces back to READY.
//  2. Intra-run DAG descendants → SKIPPED with skip_reason
//   = "upstream failed: <targetID>". Terminal, carries the
//   reason so run_status renders ⊘ "(upstream failed: X)"
//   distinctly from vote-cascade skips.
//  3. Artifact rollback — identical to invalidation. The
//   target's writes roll back to the prior accepted writer
//   (or delete if none).
//  4. Cross-run reader cascade was removed with the branch-
//   per-run model.
//  5. Dynamic-for_each descendants → DELETE (not PARK like
//   invalidate). A terminally failed source never re-accepts,
//   so parked descendants would orphan forever.
//
// The computation reuses engine.ComputeInvalidation — only the
// applied Plan differs (terminal states vs reset-to-retry).
func (c *Coordinator) PerformFailCascade(taskID, reason string) (*FailCascadeResult, error) {
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	d, err := c.Cache.GetDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG for run %d: %w", task.RunID, err)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found for task %q", taskID)
	}
	parsed, _ := c.Cache.GetParsedRun(task.RunID)

	outcome, err := engine.New(c.Store, c.Logger).ComputeInvalidation(task, run, d, parsed)
	if err != nil {
		return nil, err
	}

	var mutations []store.Mutation

	// 1. Artifact rollbacks (same as invalidation).
	var rollbacks []RollbackOutcome
	for _, rb := range outcome.ArtifactRollbacks {
		if rb.Delete {
			mutations = append(mutations, store.DeleteArtifact{
				ProjectID: rb.ProjectID,
				Branch:  rb.Branch,
				Path:   rb.Path,
			})
			rollbacks = append(rollbacks, RollbackOutcome{Path: rb.Path, Deleted: true})
		} else if rb.RestoreTo != nil {
			mutations = append(mutations, store.MoveArtifact{
				Artifact: *rb.RestoreTo,
			})
			rollbacks = append(rollbacks, RollbackOutcome{
				Path:        rb.Path,
				RestoredFromTask:  rb.RestoreTo.LastTaskID,
				RestoredFromCommit: rb.RestoreTo.CommitSHA,
			})
		}
	}

	// 2. Target → FAILED (terminal). Preserve commit_sha and
	//  claim info for audit.
	mutations = append(mutations, store.SetTaskState{
		TaskID:   taskID,
		NewState:  store.TaskFailed,
		FailReason: reason,
	})

	// 3. Intra-run descendants → SKIPPED with reason. Filter
	//  to non-terminal descendants only — already-ACCEPTED/
	//  FAILED/SKIPPED rows stay (no need to overwrite).
	skipReason := fmt.Sprintf("upstream failed: %s", taskID)
	var skippedDescendants []string
	for _, descID := range outcome.RegularDescendants {
		dt, err := c.Store.GetTask(descID)
		if err != nil || dt == nil {
			continue
		}
		if isTerminalTaskState(store.TaskState(dt.State)) {
			continue
		}
		mutations = append(mutations, store.SetTaskState{
			TaskID:   descID,
			NewState:  store.TaskSkipped,
			ClearClaim: true,
			SkipReason: skipReason,
		})
		skippedDescendants = append(skippedDescendants, descID)
	}

	// 4. Cross-run readers removed (branch isolation).

	// 5. Dynamic descendants → delete (not park).
	for _, descID := range outcome.DematerializedIDs {
		mutations = append(mutations, store.DeleteTask{TaskID: descID})
	}
	// Phase 6c — invalidate descendants' open claims (collateral).
	// Folded into the same plan so the state flips and the claim
	// outcome closures land atomically.
	for _, descID := range skippedDescendants {
		mutations = append(mutations, store.MarkOpenClaimsInvalidated{TaskID: descID})
	}

	plan := store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}.AppendCascade(task.RunID)
	// cascade_fired rides the same plan via EmitEvent so the
	// chokepoint contract holds — no out-of-band Events().Record.
	plan.Mutations = append(plan.Mutations, store.EmitEvent{Event: store.Event{
		EventType:    "cascade_fired",
		EventSubtype: "fail",
		TaskID:       taskID,
		RunID:        task.RunID,
		ProjectID:    run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"reason":            reason,
			"descendants_count": len(skippedDescendants),
			"dematerialized":    len(outcome.DematerializedIDs),
			"rollbacks":         len(rollbacks),
		}),
		CreatedAt: time.Now(),
	}})
	result, err := c.Store.ApplyPlan(plan)
	if err != nil {
		return nil, err
	}

	if len(outcome.DematerializedDefs) > 0 {
		c.Cache.Invalidate(task.RunID)
	}

	c.EvaluateRunStateAndMaybeTriage(task.RunID)

	return &FailCascadeResult{
		Task:        task,
		Reason:       reason,
		SkippedDescendants: skippedDescendants,
		Dematerialized:   outcome.DematerializedIDs,
		Changed:      result.Changed + result.TasksDeleted,
		Rollbacks:     rollbacks,
	}, nil
}

// isTerminalTaskState reports whether a task state is terminal.
// Used by PerformFailCascade to avoid overwriting the ACCEPTED
// review that caused the failure, or descendants that already
// landed in their own terminal state.
func isTerminalTaskState(s store.TaskState) bool {
	switch s {
	case store.TaskAccepted, store.TaskFailed, store.TaskSkipped:
		return true
	}
	// TaskFailedRetryable is deliberately NOT terminal — a
	// compute task that errored is a live blocker the operator
	// will retry, not a settled outcome. Do not add it to the
	// case above; the fail cascade must be able to (re)touch it.
	return false
}

// FailTaskResponse is the wire shape for enju_fail_task.
type FailTaskResponse struct {
	Status       string         `json:"status"`
	TaskID       string         `json:"task_id"`
	Reason       string         `json:"reason"`
	SkippedDescendants []string       `json:"skipped_descendants,omitempty"`
	Rollbacks      []ArtifactRollbackView `json:"rollbacks,omitempty"`
}

// failTaskOwnershipOK is the load-bearing ownership invariant for
// FailTask, extracted as a pure function so the three-way truth
// table is unit-pinned independent of the store/cascade.
//
// Rule: a BOT may only FAIL a task it currently holds the claim
// on. Humans keep operator-override (FailTask's CanReadProject
// gate) so a person can kill a wedged task they don't own.
// Without this, a bot that merely *couldn't claim* a task (wrong
// require_role, its own model misconfig) could drive that task —
// and every descendant via the fail cascade — through FAILED,
// terminating work meant for the right citizen who never got a
// turn. The process+submit budget is sound precisely because a
// successful claim already established ownership; the claim path
// has none. "I can't take this" must never mean "this is
// broken." Path-independent — defends against any non-owning
// caller, not just the daemon's claim path.
func failTaskOwnershipOK(caller *store.CitizenRecord, task *store.TaskRecord) error {
	if caller.Kind == store.CitizenKindAgent && task.ClaimedBy != caller.ID {
		return fmt.Errorf(
			"%w: bot %q is not the claimant of task %q (claimed_by=%d); a bot may only fail a task it holds",
			ErrForbidden, caller.Username, task.ID, task.ClaimedBy)
	}
	return nil
}

// FailTask is the operator-facing wrapper around
// PerformFailCascade. Validates the target via engine.ComputeFailTask
// (state precondition), gates on project membership, runs the
// cascade, and records the task_failed contribution event.
func (c *Coordinator) FailTask(caller *store.CitizenRecord, taskID, reason string) (*FailTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalidArgument)
	}
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%w: task %q not found", ErrNotFound, taskID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("%w: run for task %q not found", ErrNotFound, taskID)
	}
	if !CanReadProject(c.Store, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrForbidden)
	}

	if err := failTaskOwnershipOK(caller, task); err != nil {
		return nil, err
	}

	// Engine validates state precondition before we cascade.
	if _, err := engine.New(c.Store, c.Logger).ComputeFailTask(taskID, reason); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}

	res, err := c.PerformFailCascade(taskID, reason)
	if err != nil {
		return nil, err
	}

	// Record contribution event against the claimant if known
	// (matches the legacy api shape).
	if updated, _ := c.Store.GetTask(taskID); updated != nil && updated.ClaimedBy > 0 {
		c.Store.RecordContributionEvent(&store.ContributionEvent{
			CitizenID: updated.ClaimedBy,
			EventType: "task_failed",
			TaskID:  taskID,
			RunID:   updated.RunID,
			ProjectID: run.ProjectID,
			Metadata: store.MarshalMetadata(map[string]any{"reason": reason}),
			CreatedAt: time.Now(),
		})
	}

	rb := make([]ArtifactRollbackView, 0, len(res.Rollbacks))
	for _, r := range res.Rollbacks {
		v := ArtifactRollbackView{Path: r.Path}
		if r.Deleted {
			v.Deleted = true
		} else {
			v.RestoredFromTask = r.RestoredFromTask
			v.RestoredFromCommit = r.RestoredFromCommit
		}
		rb = append(rb, v)
	}
	return &FailTaskResponse{
		Status:       "failed",
		TaskID:       taskID,
		Reason:       reason,
		SkippedDescendants: res.SkippedDescendants,
		Rollbacks:      rb,
	}, nil
}

// FailComputeTaskRetryable is the NON-terminal sibling of
// FailTask, for a compute task whose script exited non-zero (the
// /fail kind="compute_error" path). The task parks as
// failed_retryable instead of FAILED: the run stays alive,
// descendants are NOT skip-cascaded (they stay PENDING, blocked
// on this task by ordinary dependency-not-satisfied — see the
// failed_retryable doc), and the operator recovers with
// enju_retry_task. The failed attempt's partial writes are still
// rolled back (same as the terminal cascade) so a retry starts
// from a clean upstream state.
//
// Strict precondition: only an actually-running compute task can
// enter this path. Anything else (a review, a non-running task,
// an explicit operator fail) goes through the terminal FailTask —
// "errored" must not be confused with "rejected".
func (c *Coordinator) FailComputeTaskRetryable(caller *store.CitizenRecord, taskID, reason string) (*FailTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	if reason == "" {
		return nil, fmt.Errorf("%w: reason is required", ErrInvalidArgument)
	}
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("%w: task %q not found", ErrNotFound, taskID)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("%w: run for task %q not found", ErrNotFound, taskID)
	}
	if !CanReadProject(c.Store, run.ProjectID, caller.ID) {
		return nil, fmt.Errorf("%w: not a member of this project", ErrForbidden)
	}
	if err := failTaskOwnershipOK(caller, task); err != nil {
		return nil, err
	}
	if task.Script == "" {
		return nil, fmt.Errorf("%w: compute_error is only valid for a compute task (task %q has no script)", ErrInvalidArgument, taskID)
	}
	st := store.TaskState(task.State)
	if st != store.TaskClaimed && st != store.TaskRunning {
		return nil, fmt.Errorf("%w: task %q is %s, not running — compute_error requires a running compute task", ErrInvalidArgument, taskID, st)
	}

	// Auto-retry budget (Snakemake retries:): a compute_error within
	// the task's retries budget re-admits to READY for another attempt
	// instead of parking failed_retryable. The attempt number is the
	// failing claim's iter_seq (claim 1 = first attempt), so a failure
	// at iter_seq <= Retries grants a re-run; beyond it, fall through
	// to the park below. The duplicate-report case is handled by the
	// CLAIMED/RUNNING precondition above: once re-admitted the task is
	// READY, so a re-posted failure for the same attempt is rejected.
	if task.Retries > 0 {
		if iter, ierr := c.Store.GetOpenClaimIterSeq(taskID); ierr == nil && iter > 0 && int(iter) <= task.Retries {
			if aerr := c.performComputeAutoRetry(taskID, reason, int(iter), task.Retries); aerr != nil {
				return nil, aerr
			}
			c.Logger.Info("compute task auto-retried",
				"task_id", taskID, "attempt", iter, "of", task.Retries, "reason", reason)
			return &FailTaskResponse{Status: "retrying", TaskID: taskID, Reason: reason}, nil
		}
	}

	rollbacks, err := c.performComputeFailure(taskID, reason)
	if err != nil {
		return nil, err
	}

	if updated, _ := c.Store.GetTask(taskID); updated != nil && updated.ClaimedBy > 0 {
		c.Store.RecordContributionEvent(&store.ContributionEvent{
			CitizenID: updated.ClaimedBy,
			EventType: "task_failed",
			TaskID:  taskID,
			RunID:   updated.RunID,
			ProjectID: run.ProjectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"reason": reason, "kind": "compute_error", "retryable": true,
			}),
			CreatedAt: time.Now(),
		})
	}

	rb := make([]ArtifactRollbackView, 0, len(rollbacks))
	for _, r := range rollbacks {
		v := ArtifactRollbackView{Path: r.Path}
		if r.Deleted {
			v.Deleted = true
		} else {
			v.RestoredFromTask = r.RestoredFromTask
			v.RestoredFromCommit = r.RestoredFromCommit
		}
		rb = append(rb, v)
	}
	return &FailTaskResponse{
		Status:    "failed_retryable",
		TaskID:    taskID,
		Reason:    reason,
		Rollbacks: rb,
	}, nil
}

// performComputeFailure applies the retryable Plan: roll back the
// failed attempt's partial writes (reusing ComputeInvalidation's
// rollback computation — identical to the terminal cascade's step
// 1), flip the target to failed_retryable + close its claim, emit
// cascade_fired{compute_error}. It deliberately does NOT touch
// regular or dynamic descendants — leaving them PENDING is what
// keeps the run a WAITING, retryable state instead of a dead one.
func (c *Coordinator) performComputeFailure(taskID, reason string) ([]RollbackOutcome, error) {
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	d, err := c.Cache.GetDAG(task.RunID)
	if err != nil {
		return nil, fmt.Errorf("loading DAG for run %d: %w", task.RunID, err)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return nil, fmt.Errorf("run not found for task %q", taskID)
	}
	parsed, _ := c.Cache.GetParsedRun(task.RunID)

	outcome, err := engine.New(c.Store, c.Logger).ComputeInvalidation(task, run, d, parsed)
	if err != nil {
		return nil, err
	}

	var mutations []store.Mutation
	var rollbacks []RollbackOutcome
	for _, rbk := range outcome.ArtifactRollbacks {
		if rbk.Delete {
			mutations = append(mutations, store.DeleteArtifact{
				ProjectID: rbk.ProjectID, Branch: rbk.Branch, Path: rbk.Path,
			})
			rollbacks = append(rollbacks, RollbackOutcome{Path: rbk.Path, Deleted: true})
		} else if rbk.RestoreTo != nil {
			mutations = append(mutations, store.MoveArtifact{Artifact: *rbk.RestoreTo})
			rollbacks = append(rollbacks, RollbackOutcome{
				Path:        rbk.Path,
				RestoredFromTask:  rbk.RestoreTo.LastTaskID,
				RestoredFromCommit: rbk.RestoreTo.CommitSHA,
			})
		}
	}

	// Close the failed attempt's iteration ledger row with
	// outcome='failed'. SetTaskState{ClearClaim} only clears the
	// task-level claim pointer (tasks columns); it does NOT close
	// the task_claims row. Without this the failed attempt's row
	// stays open (outcome IS NULL), so a later enju_retry_task
	// re-claim would REUSE its iter_seq instead of advancing —
	// collapsing the retry into the failed attempt instead of
	// recording it as its own auditable iteration.
	mutations = append(mutations, store.MarkOpenClaimsFailed{TaskID: taskID})

	mutations = append(mutations, store.SetTaskState{
		TaskID:   taskID,
		NewState:  store.TaskFailedRetryable,
		FailReason: reason,
		ClearClaim: true, // drop the task-level claim pointer
	})

	plan := store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}.AppendCascade(task.RunID)
	plan.Mutations = append(plan.Mutations, store.EmitEvent{Event: store.Event{
		EventType:    "cascade_fired",
		EventSubtype: "compute_error",
		TaskID:       taskID,
		RunID:        task.RunID,
		ProjectID:    run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"reason":    reason,
			"retryable": true,
			"rollbacks": len(rollbacks),
		}),
		CreatedAt: time.Now(),
	}})
	if _, err := c.Store.ApplyPlan(plan); err != nil {
		return nil, err
	}
	// Recompute run state: with the target failed_retryable and
	// descendants left PENDING, applyCompleteRun lands on WAITING
	// (failed_retryable counts as holding), never terminal.
	c.EvaluateRunStateAndMaybeTriage(task.RunID)
	return rollbacks, nil
}

// performComputeAutoRetry re-admits a transiently-failed compute task
// to READY for another attempt instead of parking it failed_retryable
// — the Snakemake `retries:` budget. Same rollback of the failed
// attempt's partial writes as performComputeFailure (so the re-run
// starts clean) and the same claim-ledger close (so the next claim
// advances iter_seq, the attempt counter), but the task ends READY.
//
// The RUNNING → failed_retryable → READY transition runs as ONE
// ApplyPlan: the intermediate park is mandatory (the ClearClaim→READY
// gate only permits {accepted,submitted,failed,failed_retryable}→READY,
// not RUNNING directly), and doing both in one transaction keeps the
// run from being observed in the transient failed_retryable/WAITING
// state by the single EvaluateRunStateAndMaybeTriage below (which sees
// the final READY → ACTIVE and so never spurious-triages).
func (c *Coordinator) performComputeAutoRetry(taskID, reason string, attempt, budget int) error {
	task, err := c.Store.GetTask(taskID)
	if err != nil || task == nil {
		return fmt.Errorf("task %q not found", taskID)
	}
	d, err := c.Cache.GetDAG(task.RunID)
	if err != nil {
		return fmt.Errorf("loading DAG for run %d: %w", task.RunID, err)
	}
	run, err := c.Store.GetRun(task.RunID)
	if err != nil || run == nil {
		return fmt.Errorf("run not found for task %q", taskID)
	}
	parsed, _ := c.Cache.GetParsedRun(task.RunID)
	claimant := task.ClaimedBy

	outcome, err := engine.New(c.Store, c.Logger).ComputeInvalidation(task, run, d, parsed)
	if err != nil {
		return err
	}

	var mutations []store.Mutation
	rolledBack := 0
	for _, rbk := range outcome.ArtifactRollbacks {
		if rbk.Delete {
			mutations = append(mutations, store.DeleteArtifact{ProjectID: rbk.ProjectID, Branch: rbk.Branch, Path: rbk.Path})
			rolledBack++
		} else if rbk.RestoreTo != nil {
			mutations = append(mutations, store.MoveArtifact{Artifact: *rbk.RestoreTo})
			rolledBack++
		}
	}
	mutations = append(mutations,
		store.MarkOpenClaimsFailed{TaskID: taskID},
		// RUNNING → failed_retryable (closes the claim, records reason).
		store.SetTaskState{TaskID: taskID, NewState: store.TaskFailedRetryable, FailReason: reason, ClearClaim: true},
		// failed_retryable → READY (the re-admit; same as enju_retry_task).
		store.SetTaskState{TaskID: taskID, NewState: store.TaskReady, ClearClaim: true},
		store.EmitEvent{Event: store.Event{
			CitizenID: claimant,
			EventType: "auto_retry",
			TaskID:    taskID,
			RunID:     task.RunID,
			ProjectID: run.ProjectID,
			Metadata: store.MarshalMetadata(map[string]any{
				"attempt":   attempt,
				"of":        budget,
				"reason":    reason,
				"rollbacks": rolledBack,
			}),
			CreatedAt: time.Now(),
		}},
	)
	if _, err := c.Store.ApplyPlan(store.Plan{Version: engine.EngineVersion, Mutations: mutations}); err != nil {
		return err
	}
	// Re-admitted task is now READY (an active blocker) → recompute run
	// state so WAITING flips back to ACTIVE.
	c.EvaluateRunStateAndMaybeTriage(task.RunID)
	return nil
}
