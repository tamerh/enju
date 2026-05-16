package engine

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ComputeStartTask validates that a task can transition to RUNNING
// and returns a Plan that flips it to RUNNING and re-anchors its
// claim lease. Pure computation — reads state, never writes.
//
// Allowed only from CLAIMED. The transition is fired by the
// fat-client (compute or bot) right before kicking off the script
// or LLM call, providing a "claimed but not yet running" vs
// "actually running" diagnostic. Other states (READY, PENDING,
// SUBMITTED, ACCEPTED, FAILED, ...) are rejected because they
// either haven't reached the claim phase yet or are past it.
//
// deadline re-anchors the open claim's lease so it covers the
// task's actual execution (which begins at RUNNING) instead of
// the claim-time guess — the caller passes now + the task's
// timeout. Without this the reaper treated a long legitimate task
// (a multi-hour assembly, or a first-run multi-GB image pull)
// identically to a dead worker, since both just look like a stale
// claim-time deadline on a claimed/running row.
//
// Boundary: only the post-RUNNING budget is re-anchored. The
// claim → /started window itself stays on the original claim-time
// lease — but that window is just "fat-client claimed, assembled
// the spec/env, posted started" (sub-second; all I/O-heavy work
// is post-RUNNING), so the ≥30-min default / declared timeout
// covers it with enormous margin. Not re-anchoring from the
// instant of claim is intentional, not an oversight.
//
// Mirrors ComputeFailTask's shape so the API/service plumbing
// stays uniform across observability state-flip endpoints.
func (e *Engine) ComputeStartTask(taskID string, deadline time.Time) (*store.Plan, error) {
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
			store.SetClaimDeadline{
				TaskID:   taskID,
				Deadline: deadline,
			},
		},
	}, nil
}
