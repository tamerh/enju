package service

import (
	"fmt"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// PauseRunResponse is the wire shape for enju_pause_run /
// POST /projects/{p}/runs/{r}/pause. Carries the new state +
// the changed flag so callers can distinguish a fresh pause
// from an already-paused no-op.
type PauseRunResponse struct {
	Status  string `json:"status"`           // "paused" | "already_paused"
	RunID   string `json:"run_id"`           // user-facing "{project}:{seq}"
	State   string `json:"state"`            // current run state
	Changed bool   `json:"changed"`          // true if this call flipped the bit
	Message string `json:"message,omitempty"`
}

// PauseRun pauses a run. Membership-gated through the run's
// parent project (legacy zero-member projects stay open).
// Returns ErrNotFound when the run doesn't exist;
// ErrNotMember when the caller can't write to the project.
func PauseRun(s *store.Store, caller *store.CitizenRecord, projectID int64, runSeq int) (*PauseRunResponse, error) {
	run, err := s.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	// Snapshot the prior state so we can report changed=true
	// only when we actually flipped active|idle → paused.
	priorState := string(run.State)
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.PauseRun{RunID: run.ID, CitizenID: caller.ID},
		},
	}); err != nil {
		return nil, err
	}
	updated, _ := s.GetRun(run.ID)
	state := ""
	if updated != nil {
		state = string(updated.State)
	}
	changed := priorState != state
	status := "paused"
	if !changed {
		status = "already_paused"
	}
	return &PauseRunResponse{
		Status:  status,
		RunID:   fmt.Sprintf("%d:%d", projectID, runSeq),
		State:   state,
		Changed: changed,
		Message: "run paused — SpawnTask now refuses on paused runs, but claims and submits still pass through.",
	}, nil
}

// ResumeRunResponse is the wire shape for enju_resume_run /
// POST /projects/{p}/runs/{r}/resume.
type ResumeRunResponse struct {
	Status string `json:"status"` // "resumed"
	RunID  string `json:"run_id"`
	State  string `json:"state"` // "active" or "idle" depending on ready work
}

// ResumeRun lifts a paused run back to active or idle,
// depending on whether ready tasks exist. Membership-gated.
// Refuses on terminal runs (Store.ResumeRun returns the error).
func ResumeRun(s *store.Store, caller *store.CitizenRecord, projectID int64, runSeq int) (*ResumeRunResponse, error) {
	run, err := s.GetRunByProjectSeq(projectID, runSeq)
	if err != nil {
		return nil, err
	}
	if run == nil {
		return nil, ErrNotFound
	}
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	// ResumeRun lifts paused → active. The follow-on CompleteRun
	// in the same plan re-evaluates the task graph so the run
	// lands on active or idle (or even completed if every task
	// is already terminal). Single transaction, single drain.
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.ResumeRun{RunID: run.ID, CitizenID: caller.ID},
			store.CompleteRun{RunID: run.ID},
		},
	}); err != nil {
		return nil, err
	}
	updated, _ := s.GetRun(run.ID)
	state := ""
	if updated != nil {
		state = string(updated.State)
	}
	return &ResumeRunResponse{
		Status: "resumed",
		RunID:  fmt.Sprintf("%d:%d", projectID, runSeq),
		State:  state,
	}, nil
}
