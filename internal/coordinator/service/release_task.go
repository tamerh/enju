package service

import (
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReleaseTaskResponse is the wire shape for enju_release_task /
// POST /tasks/{id}/release.
type ReleaseTaskResponse struct {
	Status string `json:"status"` // "released"
}

// ReleaseTask releases a task back to ready state. Caller must
// be a member of the task's parent project.
func ReleaseTask(s *store.Store, caller *store.CitizenRecord, taskID string) (*ReleaseTaskResponse, error) {
	task, err := s.GetTask(taskID)
	if err != nil {
		return nil, err
	}
	if task == nil {
		return nil, ErrNotFound
	}
	run, err := s.GetRun(task.RunID)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, run.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	// Released task goes CLAIMED → READY. No cascade needed:
	// the released task IS the one becoming ready; nothing
	// downstream unblocks because no upstream just resolved.
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.ReleaseClaim{TaskID: taskID, CitizenID: caller.ID},
		},
	}); err != nil {
		return nil, err
	}
	return &ReleaseTaskResponse{Status: "released"}, nil
}
