package engine

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ComputeHeartbeatTask validates that a task's open claim can be
// renewed and returns a Plan that re-anchors the claim deadline.
// Pure computation — reads state, never writes.
//
// The lease/reaper pair exists to recover tasks whose WORKER died,
// but the lease length is a guess (declared timeout, or the 30-min
// default). A legitimate sync compute script can run for hours —
// far past any reasonable guess — and without renewal the reaper
// treated it identically to a dead worker: claim expired, task
// re-readied, and the script's eventual result refused (the
// longtask-parallel-run-desync bug). The heartbeat turns the lease
// into a liveness signal: the fat-client re-anchors periodically
// while the script runs, so expiry again means "worker went
// silent", not "script was slow".
//
// Allowed from CLAIMED or RUNNING — the renewal loop starts
// around the whole execution window and must not race the
// /started transition. Any other state means the claim is gone
// (reaped, released, resolved) and the renewal is refused so the
// client can see its lease was lost instead of silently pinging
// a claim that no longer exists.
//
// Mirrors ComputeStartTask's shape so the API/service plumbing
// stays uniform across claim-lifecycle endpoints.
func (e *Engine) ComputeHeartbeatTask(taskID string, deadline time.Time) (*store.Plan, error) {
	task, err := e.store.GetTask(taskID)
	if err != nil || task == nil {
		return nil, fmt.Errorf("task %q not found", taskID)
	}
	st := store.TaskState(task.State)
	if st != store.TaskClaimed && st != store.TaskRunning {
		return nil, fmt.Errorf("task %q cannot renew its claim lease (state: %s, must be claimed or running)",
			taskID, StateLabel(st))
	}
	return &store.Plan{
		Version: EngineVersion,
		Mutations: []store.Mutation{
			store.SetClaimDeadline{
				TaskID:   taskID,
				Deadline: deadline,
			},
		},
	}, nil
}
