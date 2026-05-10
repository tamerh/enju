package engine

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ComputeFailTask validates that a task can be failed and
// returns a Plan with a SetTaskState(→FAILED) mutation.
// Pure computation — reads state, never writes.
//
// A task can be failed from CLAIMED, RUNNING, SUBMITTED, READY,
// COLLECTING, or ACCEPTED states. Coverage notes:
//   - RUNNING (Phase 8.2): the async-compute reaper path posts
//     /fail on a wrapper that dropped a non-zero .wrap-result.json
//     after the CLAIMED → RUNNING transition before kickoff.
//   - SUBMITTED (Phase 8.3): the review-reject cascade path
//     fails the upstream target task whose state is now
//     SUBMITTED rather than ACCEPTED. Same path also fires when
//     a citizen-action submit triggers a fail-cascade on
//     downstream task while their own task is still SUBMITTED.
//   - ACCEPTED stays for the "operator hard-rejects already-
//     merged work" admin path. Terminal states (skipped,
//     failed) are rejected.
// The reason is stored on the task record so citizens can see
// why it failed in run_status and get_task.
func (e *Engine) ComputeFailTask(taskID, reason string) (*store.Plan, error) {
	task, err := e.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	switch store.TaskState(task.State) {
	case store.TaskClaimed, store.TaskRunning, store.TaskSubmitted, store.TaskReady, store.TaskCollecting, store.TaskAccepted:
		// OK — task is in a failable state.
	default:
		return nil, fmt.Errorf("task %q cannot be failed (state: %s)", taskID, StateLabel(store.TaskState(task.State)))
	}

	return &store.Plan{
		Version: EngineVersion,
		Mutations: []store.Mutation{
			store.SetTaskState{
				TaskID:     taskID,
				NewState:   store.TaskFailed,
				FailReason: reason,
			},
		},
	}, nil
}
