package engine

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ComputeStartTask validates that a task can transition to RUNNING
// and returns a Plan with a SetTaskState(→RUNNING) mutation. Pure
// computation — reads state, never writes.
//
// Allowed only from CLAIMED. The transition is fired by the
// fat-client (compute or bot) right before kicking off the script
// or LLM call, providing a "claimed but not yet running" vs
// "actually running" diagnostic. Other states (READY, PENDING,
// SUBMITTED, ACCEPTED, FAILED, ...) are rejected because they
// either haven't reached the claim phase yet or are past it.
//
// Mirrors ComputeFailTask's shape so the API/service plumbing
// stays uniform across observability state-flip endpoints.
func (e *Engine) ComputeStartTask(taskID string) (*store.Plan, error) {
	task, err := e.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	if store.TaskState(task.State) != store.TaskClaimed {
		return nil, fmt.Errorf("task %q cannot be marked running (state: %s, must be claimed)",
			taskID, StateLabel(store.TaskState(task.State)))
	}
	return &store.Plan{
		Version: EngineVersion,
		Mutations: []store.Mutation{
			store.SetTaskState{
				TaskID:   taskID,
				NewState: store.TaskRunning,
			},
		},
	}, nil
}
