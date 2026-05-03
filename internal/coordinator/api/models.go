package api

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
)

// handleListModels returns the model catalog. Open to any
// authenticated citizen — the catalog is public information; you
// need to know which models exist before you can attribute work to
// them.
func (s *Server) handleListModels(w http.ResponseWriter, r *http.Request) {
	resp, err := service.ListModels(s.store)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list models: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type registerModelRequest struct {
	Username  string `json:"username"`        // required, slug-form
	DisplayName string `json:"display_name,omitempty"` // optional, defaults to username
}

// handleRegisterModel creates a new kind='model' citizen in the
// catalog. Per the design doc's "free-form + soft validation"
// stance, any authenticated citizen can register in local mode;
// hosted-mode policy gating is deferred. Idempotent on duplicate
// (returns 409 with a helpful error).
func (s *Server) handleRegisterModel(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req registerModelRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.RegisterModel(s.store, caller, req.Username, req.DisplayName)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}
