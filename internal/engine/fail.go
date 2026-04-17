package engine

import (
	"fmt"

	"github.com/enju-ai/enju/internal/store"
)

// ComputeFailTask validates that a task can be failed and
// returns a Plan with a SetTaskState(→FAILED) mutation.
// Pure computation — reads state, never writes.
//
// A task can be failed from CLAIMED, READY, COLLECTING, or
// ACCEPTED states. ACCEPTED is included because a reviewer
// can hard-reject work that was already submitted. Terminal
// states (skipped, failed) are rejected. The reason is stored
// on the task record so citizens can see why it failed in
// run_status and get_task.
func (e *Engine) ComputeFailTask(taskID, reason string) (*store.Plan, error) {
	task, err := e.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}

	switch store.TaskState(task.State) {
	case store.TaskClaimed, store.TaskReady, store.TaskCollecting, store.TaskAccepted:
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
