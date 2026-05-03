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

	plan := store.Plan{
		Version:  engine.EngineVersion,
		Mutations: mutations,
	}
	result, err := c.Store.ApplyPlan(plan)
	if err != nil {
		return nil, err
	}

	// Phase 6c — invalidate descendants' open claims (collateral).
	for _, descID := range skippedDescendants {
		if _, err := c.Store.MarkOpenClaimsInvalidated(descID); err != nil {
			c.Logger.Debug("invalidate descendant open claims (fail-cascade)",
				"task_id", descID, "error", err)
		}
	}

	if len(outcome.DematerializedDefs) > 0 {
		c.Cache.Invalidate(task.RunID)
	}

	_, _ = c.Store.UpdateReadyTasks(task.RunID)
	c.EvaluateRunStateAndMaybeTriage(task.RunID)

	c.Store.Events().Record(store.Event{
		EventType:  "cascade_fired",
		EventSubtype: "fail",
		TaskID:    taskID,
		RunID:    task.RunID,
		ProjectID:  run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"reason":      reason,
			"descendants_count": len(skippedDescendants),
			"dematerialized":  len(outcome.DematerializedIDs),
			"rollbacks":     len(rollbacks),
		}),
		CreatedAt: time.Now(),
	})

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
