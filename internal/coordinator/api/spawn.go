package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/go-chi/chi/v5"
)

// --- Spawn primitive (living-workflow phase 4a) ---

type spawnTaskRequest struct {
	ParentTaskID string  `json:"parent_task_id,omitempty"`
	TaskDefID  string  `json:"task_def_id"`
	Action    string  `json:"action"`
	Prompt    string  `json:"prompt,omitempty"`
	UserPrompt  string  `json:"user_prompt,omitempty"`
	Citizens   int   `json:"citizens,omitempty"`
	DependsOn  []string `json:"depends_on,omitempty"`
	AssignTo   []string `json:"assign_to,omitempty"`
	RequireRole string  `json:"require_role,omitempty"`
	ResultType  string  `json:"result_type,omitempty"`
	Trigger   string  `json:"trigger,omitempty"`
}

// handleSpawnTask creates a new task in an existing run at
// runtime. Member-gated; the spawning citizen is the
// authenticated caller. Subject to the per-run cycle budget —
// budget exhaustion auto-pauses the run and returns 409 Conflict
// so callers can distinguish "you tried to spawn into a stopped
// run" from generic 400 validation errors.
//
// Living-workflow phase 4a. The YAML-rule sugar
// (on_review_reject, on_idle) lands in 4b/4c on top of this
// primitive.
func (s *Server) handleSpawnTask(w http.ResponseWriter, r *http.Request) {
	projectID, err := strconv.ParseInt(chi.URLParam(r, "projectID"), 10, 64)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid project_id")
		return
	}
	runSeq, err := strconv.Atoi(chi.URLParam(r, "runSeq"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid run_seq")
		return
	}
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	var req spawnTaskRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}

	resp, err := service.SpawnTask(s.store, caller, projectID, runSeq, service.SpawnTaskParams{
		ParentTaskID: req.ParentTaskID,
		TaskDefID:  req.TaskDefID,
		Action:    req.Action,
		Prompt:    req.Prompt,
		UserPrompt:  req.UserPrompt,
		Citizens:   req.Citizens,
		DependsOn:  req.DependsOn,
		AssignTo:   req.AssignTo,
		RequireRole: req.RequireRole,
		ResultType:  req.ResultType,
		Trigger:   req.Trigger,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrCycleBudgetExhausted):
			// 409 Conflict: the run is now paused; caller
			// extends budget and resumes.
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		case errors.Is(err, service.ErrNotMember), errors.Is(err, service.ErrForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		default:
			writeError(w, http.StatusBadRequest, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
