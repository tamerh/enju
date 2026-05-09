package service

import (
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ReleaseTaskResponse is the wire shape for enju_release_task /
// POST /tasks/{id}/release.
type ReleaseTaskResponse struct {
	Status store.ClaimOutcome `json:"status"` // always ClaimOutcomeReleased
}

// ReleaseTask releases a task back to ready state. Caller must
// be a member of the task's parent project.
func ReleaseTask(s store.CoordinatorStore, caller *store.CitizenRecord, taskID string) (*ReleaseTaskResponse, error) {
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
	return &ReleaseTaskResponse{Status: store.ClaimOutcomeReleased}, nil
}

// ReleaseAllOpenClaimsResponse is the wire shape for
// POST /api/v1/me/release-claims.
type ReleaseAllOpenClaimsResponse struct {
	ReleasedTaskIDs []string `json:"released_task_ids"`
	Count           int      `json:"count"`
}

// ReleaseAllOpenClaims releases every open claim currently held
// by the calling citizen across all tasks. Returns the list of
// task IDs that were released and the count.
//
// Used by the bot daemon's startup recovery: when a daemon
// restarts (operator-initiated stop+start, crash, coord
// outage), it has no in-memory record of prior claims. Without
// this RPC the orphaned claims sit until reaper deadline
// (~30min) and the daemon idles in the meantime, since the
// task appears CLAIMED-by-self in the ready-task scan and
// gets skipped.
//
// All releases happen in a single ApplyPlan transaction —
// either every claim is freed or none is. That keeps the
// claim_table consistent with what the daemon thinks happened
// (it sees the count and assumes every listed task is now READY).
func ReleaseAllOpenClaims(s store.CoordinatorStore, caller *store.CitizenRecord) (*ReleaseAllOpenClaimsResponse, error) {
	claims, err := s.ListOpenClaimsForCitizen(caller.ID)
	if err != nil {
		return nil, err
	}
	if len(claims) == 0 {
		return &ReleaseAllOpenClaimsResponse{ReleasedTaskIDs: []string{}, Count: 0}, nil
	}
	mutations := make([]store.Mutation, 0, len(claims))
	taskIDs := make([]string, 0, len(claims))
	for _, c := range claims {
		mutations = append(mutations, store.ReleaseClaim{TaskID: c.TaskID, CitizenID: caller.ID})
		taskIDs = append(taskIDs, c.TaskID)
	}
	if _, err := s.ApplyPlan(store.Plan{
		Version:   engine.EngineVersion,
		Mutations: mutations,
	}); err != nil {
		return nil, err
	}
	return &ReleaseAllOpenClaimsResponse{ReleasedTaskIDs: taskIDs, Count: len(taskIDs)}, nil
}
