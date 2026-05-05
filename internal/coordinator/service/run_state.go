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
func PauseRun(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int) (*PauseRunResponse, error) {
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
func ResumeRun(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int) (*ResumeRunResponse, error) {
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

// reasonMaxLen caps the reason field on TerminateRun. The
// reason is operator-supplied free text (often LLM-generated)
// that lands verbatim in the run_terminated event metadata.
// Bounding at ~500 chars keeps the event log from bloating on
// a too-eager assistant while still leaving room for an honest
// paragraph of explanation.
const reasonMaxLen = 500

// TerminateRunResponse is the wire shape for enju_terminate_run /
// POST /projects/{p}/runs/{r}/terminate. SkippedTasks +
// AbandonedClaims surface the cascade fan-out so the caller can
// audit "how much in-flight work just got dropped" without a
// follow-up read.
type TerminateRunResponse struct {
	Status          string `json:"status"` // "terminated"
	RunID           string `json:"run_id"` // "{project}:{seq}"
	State           string `json:"state"`  // current run state ("terminated")
	PriorState      string `json:"prior_state"`
	SkippedTasks    int    `json:"skipped_tasks"`
	AbandonedClaims int    `json:"abandoned_claims"`
	Reason          string `json:"reason,omitempty"`
}

// TerminateRun is the human-pulled-the-plug operation. Membership-
// gated through the run's parent project. Refuses on already-
// terminal runs. Pause→terminate IS valid (paused is a
// non-terminal state).
//
// Reason is optional, capped to reasonMaxLen bytes; longer
// strings are silently truncated rather than rejected so the
// caller doesn't have to count bytes — the operator's intent
// to abandon is what matters, not the exact prose length.
//
// Cascade behavior is documented on the store-side TerminateRun
// mutation: non-terminal tasks → skipped, open claims →
// abandoned, topic branches untouched, single run_terminated
// event emitted.
func TerminateRun(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, runSeq int, reason string) (*TerminateRunResponse, error) {
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
	if len(reason) > reasonMaxLen {
		reason = reason[:reasonMaxLen]
	}
	priorState := string(run.State)
	// applyTerminateRun populates result.SkippedTasks +
	// result.AbandonedClaims atomically with the cascade UPDATEs;
	// reading them off ApplyResult avoids racing the async event
	// writer for the same numbers (and the ambiguous 0,0 we'd
	// have to live with if the metadata wasn't yet visible).
	result, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.TerminateRun{RunID: run.ID, CitizenID: caller.ID, Reason: reason},
		},
	})
	if err != nil {
		return nil, err
	}
	updated, _ := s.GetRun(run.ID)
	state := ""
	if updated != nil {
		state = string(updated.State)
	}
	return &TerminateRunResponse{
		Status:          "terminated",
		RunID:           fmt.Sprintf("%d:%d", projectID, runSeq),
		State:           state,
		PriorState:      priorState,
		SkippedTasks:    result.SkippedTasks,
		AbandonedClaims: result.AbandonedClaims,
		Reason:          reason,
	}, nil
}
