package service

import (
	"fmt"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// MarkTaskRunningResponse is the wire shape for
// POST /api/v1/tasks/{id}/started.
type MarkTaskRunningResponse struct {
	Status string `json:"status"`
	TaskID string `json:"task_id"`
}

// MarkTaskRunning flips a task CLAIMED → RUNNING. The fat-client
// posts this right before kicking off compute's exec.Run or a
// bot's LLM call so the observability layer can distinguish
// "claimed but stuck" from "actually executing."
//
// Caller must be a member of the task's parent project. Engine
// pre-validates state==CLAIMED; other states return
// ErrInvalidArgument so the fat-client's best-effort retry
// logic can swallow benign races (e.g. resuming a task that
// already transitioned to RUNNING on a prior attempt).
func (c *Coordinator) MarkTaskRunning(caller *store.CitizenRecord, taskID string) (*MarkTaskRunningResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
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
	// Re-anchor the lease at RUNNING: the budget should cover the
	// task's actual execution (which starts now), not the time
	// elapsed since claim. Same timeout source as ClaimTask so the
	// two anchor points can't drift.
	deadline := time.Now().Add(taskClaimTimeout(task))
	plan, err := engine.New(c.Store, c.Logger).ComputeStartTask(taskID, deadline)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	if _, err := c.Store.ApplyPlan(*plan); err != nil {
		return nil, err
	}
	return &MarkTaskRunningResponse{Status: string(store.TaskRunning), TaskID: taskID}, nil
}

// HeartbeatTaskResponse is the wire shape for
// POST /api/v1/tasks/{id}/heartbeat.
type HeartbeatTaskResponse struct {
	Status   string    `json:"status"`
	TaskID   string    `json:"task_id"`
	Deadline time.Time `json:"deadline"`
}

// HeartbeatTask re-anchors a claimed/running task's claim lease to
// now + the task's timeout — the same duration ClaimTask and
// MarkTaskRunning anchor with, so all three lease anchor points
// share one source (taskClaimTimeout). The fat-client posts this
// periodically while a sync compute script runs so a legitimately
// long script (hours) isn't reaped at the lease guess (30-min
// default); see engine.ComputeHeartbeatTask for the full rationale.
//
// Caller must be a member of the task's parent project. Engine
// pre-validates state ∈ {CLAIMED, RUNNING}; other states return
// ErrInvalidArgument — the claim is gone (reaped/released/resolved)
// and the client should stop renewing and expect its eventual
// submit to be refused.
func (c *Coordinator) HeartbeatTask(caller *store.CitizenRecord, taskID string) (*HeartbeatTaskResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
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
	deadline := time.Now().Add(taskClaimTimeout(task))
	plan, err := engine.New(c.Store, c.Logger).ComputeHeartbeatTask(taskID, deadline)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	if _, err := c.Store.ApplyPlan(*plan); err != nil {
		return nil, err
	}
	return &HeartbeatTaskResponse{Status: string(task.State), TaskID: taskID, Deadline: deadline}, nil
}
