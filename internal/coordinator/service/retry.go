package service

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// RetryFrom selects which version of the compute script a retry
// runs. It is a coordinator-confirmed intent; the actual snapshot
// (re)materialization happens client-side at execute time.
type RetryFrom string

const (
	// RetryFromHead re-runs against the run branch's current tip
	// — the operator committed a fix to the failing script and
	// wants the next attempt to pick it up. This is the default
	// (the common case: you saw the failure, fixed the bug,
	// retried).
	RetryFromHead RetryFrom = "head"
	// RetryFromSnapshot re-runs the exact pinned snapshot script
	// unchanged — for a transient failure (flaky network, a
	// crowded box) where the code was never the problem.
	RetryFromSnapshot RetryFrom = "snapshot"
)

// RetryTaskResponse is the wire shape returned to the fat-client,
// which uses From to decide whether to re-materialize the run
// snapshot before re-executing.
type RetryTaskResponse struct {
	Status   string    `json:"status"` // always "retrying"
	TaskID   string    `json:"task_id"`
	From     RetryFrom `json:"from"`
	NewState string    `json:"new_state"` // always "ready"
	RunID    int64     `json:"run_id"`
}

// RetryTask sends a failed-but-recoverable compute task back to
// READY for a fresh attempt, WITHOUT re-running the rest of the
// run. It is the recovery half of the failed_retryable contract
// (Slices 1–2): a compute script that errored on its own merits
// parked in failed_retryable with its failed iteration already
// closed (MarkOpenClaimsFailed) and its partial artifacts already
// rolled back (performComputeFailure). So retry is deliberately
// the *minimal* composition — just re-open the target:
//
//   - No artifact rollback: already done at fail time.
//   - No claim close: already done at fail time. The next claim
//     gets a fresh iter_seq, so every retry is its own auditable
//     iteration (the trackability guarantee).
//   - No descendant cascade: descendants were left PENDING (not
//     skipped) at fail time, so flipping the target to READY is
//     all that's needed — the normal DAG flow resumes once the
//     retried attempt re-accepts.
//
// The from axis (head vs snapshot) is recorded as intent; the
// client materializes accordingly before calling execute.
func (c *Coordinator) RetryTask(caller *store.CitizenRecord, taskID string, from RetryFrom) (*RetryTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	switch from {
	case "":
		from = RetryFromHead
	case RetryFromHead, RetryFromSnapshot:
		// ok
	default:
		return nil, fmt.Errorf("%w: from must be %q or %q, got %q",
			ErrInvalidArgument, RetryFromHead, RetryFromSnapshot, from)
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

	// Precondition: only a failed_retryable task can be retried.
	// A terminal `failed` is dead (use enju_invalidate semantics
	// elsewhere); anything else isn't a failed compute attempt.
	if task.State != store.TaskFailedRetryable {
		return nil, fmt.Errorf(
			"%w: only failed_retryable tasks can be retried; task %q is %s",
			ErrInvalidArgument, taskID, task.State)
	}
	// Defensive: failed_retryable is only ever entered by a
	// compute-script failure, so Script is always set here — but
	// guard anyway so a future state-machine change can't quietly
	// route a non-compute task into a path that has no script.
	if task.Script == "" {
		return nil, fmt.Errorf(
			"%w: task %q is not a compute task (no script) — cannot retry",
			ErrInvalidArgument, taskID)
	}

	plan := store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			// Re-open the target. ClearClaim wipes the (already
			// dropped) task-level claim pointer and emits the
			// rebound-ready event so the assignee learns the work
			// is back on their plate. failed_retryable→ready is
			// permitted by applySetTaskState's ClearClaim gate.
			store.SetTaskState{
				TaskID:   taskID,
				NewState: store.TaskReady,
				ClearClaim: true,
			},
		},
	}.AppendCascade(task.RunID)
	plan.Mutations = append(plan.Mutations, store.EmitEvent{Event: store.Event{
		EventType:    "cascade_fired",
		EventSubtype: "retry",
		TaskID:       taskID,
		RunID:        task.RunID,
		ProjectID:    run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"from":      string(from),
			"retryable": true,
		}),
		CreatedAt: time.Now(),
	}})
	if _, err := c.Store.ApplyPlan(plan); err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}

	// The target left the holding bucket (failed_retryable) and
	// is now an active READY blocker — recompute run state so a
	// WAITING run flips back to ACTIVE.
	c.EvaluateRunStateAndMaybeTriage(task.RunID)

	c.Store.RecordContributionEvent(&store.ContributionEvent{
		CitizenID: caller.ID,
		EventType: "task_retried",
		TaskID:    taskID,
		RunID:     task.RunID,
		ProjectID: run.ProjectID,
		Metadata: store.MarshalMetadata(map[string]any{
			"from":            string(from),
			"retrier_citizen": caller.ID,
		}),
		CreatedAt: time.Now(),
	})

	c.Logger.Info("task retried",
		"task_id", taskID,
		"from", from,
		"run_id", task.RunID,
	)

	return &RetryTaskResponse{
		Status:   "retrying",
		TaskID:   taskID,
		From:     from,
		NewState: string(store.TaskReady),
		RunID:    task.RunID,
	}, nil
}
