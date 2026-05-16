package api

import (
	"encoding/json"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

type invalidateRequest struct {
	Reason string `json:"reason"`
}

type retryRequest struct {
	From string `json:"from,omitempty"` // "head" (default) | "snapshot"
}

// handleRetryTask sends a failed_retryable compute task back to
// READY for a fresh attempt. Mirrors handleInvalidateTask's
// caller-resolution; the from intent is forwarded verbatim and
// validated in the service layer.
func (s *Server) handleRetryTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req retryRequest
	json.NewDecoder(r.Body).Decode(&req)

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

	resp, err := s.coord.RetryTask(caller, taskID, service.RetryFrom(req.From))
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleInvalidateTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req invalidateRequest
	json.NewDecoder(r.Body).Decode(&req)

	member, ok := s.requireProjectMembershipForTask(w, r, taskID)
	if !ok {
		return
	}

	// member may be nil on legacy zero-member projects; fall
	// back to the auth-context citizen so attribution still
	// gets captured.
	var caller *store.CitizenRecord
	if member != nil {
		caller, _ = s.store.GetCitizen(member.CitizenID)
	}
	if caller == nil {
		caller = citizenFromRequest(r)
	}

	resp, err := s.coord.InvalidateTask(caller, taskID, req.Reason)
	if err != nil {
		// Not-found vs bad-state vs internal collapse into
		// service.ErrInvalidArgument; preserve the historical
		// 400 + message-as-detail behavior.
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleFailTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	var req struct {
		Reason string `json:"reason"`
		// Kind="compute_error" marks a recoverable compute-script
		// failure: the task parks as failed_retryable (run stays
		// alive, descendants stay PENDING) instead of the terminal
		// fail cascade. Set ONLY by the compute executor/reconcile;
		// operator enju_fail_task / review-reject / vote leave it
		// empty and keep the terminal path. By-construction signal,
		// not reason-string sniffing.
		Kind string `json:"kind,omitempty"`
	}
	json.NewDecoder(r.Body).Decode(&req)

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

	var resp *service.FailTaskResponse
	var err error
	if req.Kind == "compute_error" {
		resp, err = s.coord.FailComputeTaskRetryable(caller, taskID, req.Reason)
	} else {
		resp, err = s.coord.FailTask(caller, taskID, req.Reason)
	}
	if err != nil {
		writeFailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// handleTallyTask forces a tally evaluation on a collecting
// vote or review task. Any user can trigger it; it runs the
// same tally logic as a submission would, resolves if the
// threshold + quorum permit, and reports the outcome. Useful
// when a vote is stuck past its deadline or has enough
// submissions to short-circuit but nobody has submitted lately
// to re-trigger the evaluation.
func (s *Server) handleTallyTask(w http.ResponseWriter, r *http.Request) {
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
	resp, err := s.coord.TallyTask(caller, taskID)
	if err != nil {
		writeFailErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
