package api

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/service"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
)

type registerRequest struct {
	Name   string `json:"name"`
	Username string `json:"username,omitempty"` // optional, auto-generated from name if omitted
	Email  string `json:"email,omitempty"`
}

// generateUniqueUsername picks an unused username based on the display
// name. It slugifies the name, falls back to "user" if the slug is
// empty, and appends -2, -3, etc. on collision.
func (s *Server) generateUniqueUsername(displayName string) string {
	base := store.SlugifyName(displayName)
	if base == "" {
		base = "user"
	}
	candidate := base
	for i := 2; ; i++ {
		c, _ := s.store.GetCitizenByUsername(candidate)
		if c == nil {
			return candidate
		}
		candidate = fmt.Sprintf("%s-%d", base, i)
		// Safety valve — after way too many collisions, fall back to a
		// random suffix. Shouldn't ever happen in practice.
		if i > 1000 {
			return fmt.Sprintf("%s-%s", base, uuid.New().String()[:6])
		}
	}
}

func (s *Server) handleRegisterCitizen(w http.ResponseWriter, r *http.Request) {
	var req registerRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if req.Name == "" {
		writeError(w, http.StatusBadRequest, "name is required")
		return
	}
	// A human's global identity is its email — mandatory and
	// globally unique. Self-host already has it (the operator's
	// address); there is no anonymous human citizen.
	if req.Email == "" {
		writeError(w, http.StatusBadRequest, "email is required to register a human citizen")
		return
	}

	// Idempotent by the human's global identity (email). This is
	// the load-bearing fix for the registration race: a client
	// whose citizen was wiped re-registers via this auth-exempt
	// endpoint, possibly concurrently with other calls. Making the
	// ONLY writer of a human row idempotent — same email returns
	// the same citizen with a fresh token, never a 409, never a
	// duplicate — means the race cannot mint a malformed survivor
	// (the agent path separately fails closed rather than coerce).
	// One atomic ApplyPlan either way.
	if existing, lerr := s.store.GetCitizenByEmail(req.Email); lerr == nil && existing != nil {
		token := uuid.New().String()
		if _, err := s.store.ApplyPlan(store.Plan{
			Version: engine.EngineVersion,
			Mutations: []store.Mutation{
				store.IssueToken{CitizenID: existing.ID, Token: token},
			},
		}); err != nil {
			writeError(w, http.StatusInternalServerError, "re-issue token: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]interface{}{
			"id":       existing.ID,
			"username": existing.Username,
			"name":     existing.Name,
			"email":    existing.Email,
			"role":     existing.Role,
			"token":    token,
		})
		return
	}

	// Validate or generate username. An explicit username is required
	// to match the GitHub rules; an auto-generated one comes from
	// slugifying the display name and is unique by construction.
	username := req.Username
	if username != "" {
		if err := store.ValidateUsername(username); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
	} else {
		username = s.generateUniqueUsername(req.Name)
	}

	token := uuid.New().String()
	now := time.Now()

	res, err := s.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateCitizen{
				Citizen: store.CitizenRecord{
					Username:     username,
					Name:         req.Name,
					Email:        req.Email,
					Role:         "citizen",
					RegisteredAt: now,
					LastSeen:     now,
				},
				Token: token,
			},
		},
	})
	if err != nil {
		if strings.Contains(err.Error(), "email already exists") ||
			strings.Contains(err.Error(), "already taken") {
			writeError(w, http.StatusConflict, err.Error())
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to register: "+err.Error())
		return
	}
	id := res.CitizenID

	writeJSON(w, http.StatusCreated, map[string]interface{}{
		"id":    id,
		"username": username,
		"name":   req.Name,
		"email":  req.Email,
		"role":   "citizen",
		"token":  token,
	})
}

// handleGetCitizenByUsername is the user-facing citizen lookup.
func (s *Server) handleGetCitizenByUsername(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")

	citizen, err := s.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}
	writeJSON(w, http.StatusOK, citizenToMap(citizen))
}

func (s *Server) handleUpdateProfile(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		Name  *string `json:"name"`
		Email *string `json:"email"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.UpdateProfile(s.store, caller, username, req.Name, req.Email)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, service.ErrForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		case err == service.ErrNotFound:
			writeError(w, http.StatusNotFound, "citizen not found")
		default:
			writeError(w, http.StatusInternalServerError, "failed to update profile")
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCitizenContributions(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")
	resp, err := service.CitizenContributions(s.store, username)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrNotFound):
			writeError(w, http.StatusNotFound, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleCitizenDashboard(w http.ResponseWriter, r *http.Request) {
	username := chi.URLParam(r, "username")

	citizen, err := s.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		writeError(w, http.StatusNotFound, "citizen not found")
		return
	}

	active, _ := s.store.ListCitizenActiveTasks(citizen.ID)
	recent, _ := s.store.ListCitizenCompletedTasks(citizen.ID, 5)

	writeJSON(w, http.StatusOK, map[string]interface{}{
		"citizen":   citizenToMap(citizen),
		"active_tasks": s.toTaskResponses(active),
		"recent_tasks": s.toTaskResponses(recent),
	})
}

// citizenToMap renders a CitizenRecord as a map suitable for JSON
// responses. The internal int id is omitted — callers who need it can
// read `citizen.id` directly from the store.
func citizenToMap(c *store.CitizenRecord) map[string]interface{} {
	// kind defaults to "human" in the wire shape — pre-Phase-1.1
	// citizens have c.Kind="" in the DB, and the human/bot/model
	// discriminator is the v1 default for unmigrated rows.
	kind := c.Kind
	if kind == "" {
		kind = store.CitizenKindHuman
	}
	return map[string]interface{}{
		"username":      c.Username,
		"name":        c.Name,
		"email":       c.Email,
		"role":        c.Role,
		"kind":        kind,
		"score":       c.Score,
		"tasks_completed":  c.TasksCompleted,
		"tasks_timed_out":  c.TasksTimedOut,
		"tasks_released":   c.TasksReleased,
		"tokens_contributed": c.TokensContrib,
		"registered_at":   c.RegisteredAt.Format(time.RFC3339),
	}
}
