package api

import (
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/go-chi/chi/v5"
)

func (s *Server) handleReleaseTask(w http.ResponseWriter, r *http.Request) {
	taskID := chi.URLParam(r, "taskID")
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ReleaseTask(s.store, caller, taskID)
	if err != nil {
		switch err {
		case service.ErrNotFound:
			writeError(w, http.StatusNotFound, "task not found")
		case service.ErrNotMember:
			writeError(w, http.StatusForbidden, "not a member of this project")
		default:
			writeError(w, http.StatusInternalServerError, "failed to release task")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
