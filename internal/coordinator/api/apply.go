package api

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// handleApply is the unified write endpoint. Accepts a
// serialized Plan, validates the engine version, and calls
// store.ApplyPlan to execute all mutations atomically.
// Returns the ApplyResult as JSON.
func (s *Server) handleApply(w http.ResponseWriter, r *http.Request) {
	var plan store.Plan
	if err := json.NewDecoder(r.Body).Decode(&plan); err != nil {
		writeError(w, http.StatusBadRequest, "invalid plan: "+err.Error())
		return
	}

	// Version gate: reject plans from mismatched engine
	// versions so a stale client can't submit plans the
	// coordinator doesn't understand.
	if plan.Version != engine.EngineVersion {
		writeError(w, http.StatusConflict,
			fmt.Sprintf("engine version mismatch: client=%q, coordinator=%q — update your enju binary",
				plan.Version, engine.EngineVersion))
		return
	}

	result, err := s.store.ApplyPlan(plan)
	if err != nil {
		writeError(w, http.StatusBadRequest, "plan rejected: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, result)
}
