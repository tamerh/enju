package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
)

// claimRequest identifies the caller by username. Internally the
// server resolves it to an int64 citizen ID.
type claimRequest struct {
	Username string `json:"username"`
	// Model is the LLM citizen username producing the
	// words for this claim (operator/model design, layer B). Empty
	// when the operator is a human submitting unaided. Bots MUST
	// supply a non-empty value or the apply path rejects the claim.
	// Server resolves the username to a model citizen ID via
	// resolveModelByUsername.
	Model string `json:"model,omitempty"`
}

// resolveCitizen looks up a caller by username and returns the
// CitizenRecord, or writes an error response and returns nil if the
// citizen doesn't exist.
func (s *Server) resolveCitizen(w http.ResponseWriter, username string) *store.CitizenRecord {
	if username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return nil
	}
	c, err := s.store.GetCitizenByUsername(username)
	if err != nil || c == nil {
		writeError(w, http.StatusNotFound, fmt.Sprintf("citizen %q not found", username))
		return nil
	}
	return c
}

func (s *Server) handleClaimTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")

	var req claimRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if _, ok := s.requireProjectMembershipForTask(w, r, taskID); !ok {
		return
	}
	// Refuse claims on terminated runs. Same reasoning as
	// late-submit refusal — terminate is irreversible and the
	// task has already been cascade-skipped.
	if task, _ := s.store.GetTask(taskID); task != nil {
		if reason, ok := s.runTerminatedRefusal(task); ok {
			writeError(w, http.StatusConflict, reason)
			return
		}
	}

	resp, err := service.ClaimTask(s.store, s.logger, taskID, service.ClaimTaskParams{
		Username: req.Username,
		Model:  req.Model,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, service.ErrNotMember):
			writeError(w, http.StatusForbidden, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusConflict, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
