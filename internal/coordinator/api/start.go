package api

import (
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// handleMarkTaskStarted flips a task CLAIMED → RUNNING. Mirrors
// the shape of handleFailTask. POSTed by the fat-client just
// before kicking off compute or a bot's LLM call so observability
// can distinguish "claimed but stuck" from "actually running."
func (s *Server) handleMarkTaskStarted(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	member, ok := s.requireProjectMembershipForTask(w, r, taskID)
	if !ok {
		return
	}
	var caller *store.CitizenRecord
	if member != nil {
		caller, _ = s.store.GetCitizen(member.CitizenID)
	}
	if caller == nil {
		caller = citizenFromRequest(r)
	}
	resp, err := s.coord.MarkTaskRunning(caller, taskID)
	if err != nil {
		writeFailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleHeartbeatTask re-anchors a claimed/running task's claim
// lease. POSTed periodically by the fat-client while a sync
// compute script runs so a legitimately long script isn't reaped
// at the lease guess. Same plumbing shape as handleMarkTaskStarted.
func (s *Server) handleHeartbeatTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	member, ok := s.requireProjectMembershipForTask(w, r, taskID)
	if !ok {
		return
	}
	var caller *store.CitizenRecord
	if member != nil {
		caller, _ = s.store.GetCitizen(member.CitizenID)
	}
	if caller == nil {
		caller = citizenFromRequest(r)
	}
	resp, err := s.coord.HeartbeatTask(caller, taskID)
	if err != nil {
		writeFailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
