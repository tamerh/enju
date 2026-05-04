package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
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

// resolveModelByUsername looks up a model citizen by username and
// returns its ID. Empty input → (nil, nil), the unaided-human case
// (apply-layer enforces "bots must have model" so empty for a bot
// fails downstream with a clear message).
//
// Per the operator/model design doc's "free-form + soft validation"
// stance, unknown-but-valid model names are AUTO-REGISTERED as new
// kind='model' catalog entries on first use. This matches local-mode
// philosophy: someone running Ollama with a custom finetune
// shouldn't have to ceremonially pre-register before they can submit.
// Hosted-mode policy gating (require pre-registration / admin
// approval) is deferred — see the design doc.
//
// The one defense kept: reject if the username resolves to a
// non-model citizen (kind ∈ {human, bot}). Without this, a caller
// could attribute their submit to a teammate's account, blurring
// who-did-what.
func (s *Server) resolveModelByUsername(modelName string) (*int64, error) {
	if modelName == "" {
		return nil, nil
	}
	c, err := s.store.GetCitizenByUsername(modelName)
	if err != nil {
		return nil, fmt.Errorf("look up model %q: %w", modelName, err)
	}
	if c != nil {
		if c.Kind != store.CitizenKindModel {
			return nil, fmt.Errorf("citizen %q has kind %q, not %q — operators cannot be self-attributed as their own model", modelName, c.Kind, "model")
		}
		return &c.ID, nil
	}
	// Unknown model — auto-register. Display name defaults to the
	// username; explicit registration via enju_register_model gives
	// callers a chance to set a prettier display name.
	//
	// Known limitation: typos pollute the catalog. A submit with
	// model=clude-opus-4-7 (typo) creates a permanent ghost entry.
	// No cleanup tool ships today. Acceptable for now since
	// (a) the catalog is small, (b) typos surface in
	// enju_list_models so the user can spot them, (c) ghost models
	// don't authenticate or grant access. A "delete unused model"
	// admin tool can land later if catalog hygiene becomes a real
	// problem in hosted mode.
	now := time.Now()
	res, err := s.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateCitizen{Citizen: store.CitizenRecord{
				Username:     modelName,
				Name:         modelName,
				Role:         "citizen",
				Token:        "model:" + modelName,
				RegisteredAt: now,
				LastSeen:     now,
				Kind:         store.CitizenKindModel,
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("auto-register model %q: %w", modelName, err)
	}
	id := res.CitizenID
	return &id, nil
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
