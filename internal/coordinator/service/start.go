package service

import (
	"fmt"

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
	plan, err := engine.New(c.Store, c.Logger).ComputeStartTask(taskID)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
	}
	if _, err := c.Store.ApplyPlan(*plan); err != nil {
		return nil, err
	}
	return &MarkTaskRunningResponse{Status: string(store.TaskRunning), TaskID: taskID}, nil
}
