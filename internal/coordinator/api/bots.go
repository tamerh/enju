package api

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"

	"github.com/enju-ai/enju/internal/coordinator/service"
)

// --- operator/model design: bot + model registration ---
//
// Five handlers covering bot lifecycle (register, list, revoke
// token) and model catalog (list, register). All require Bearer
// auth — bots inherit the same auth surface as humans, models
// require an authenticated caller in local mode (hosted-mode
// gating deferred per the design doc).

type registerBotRequest struct {
	Name   string `json:"name"`        // display name, required
	Username string `json:"username,omitempty"` // optional — auto-slugified
	Role   string `json:"role,omitempty"`   // optional — defaults to 'citizen'
	Label  string `json:"label,omitempty"`  // optional initial-token label
}

// handleRegisterBot creates a new kind='bot' citizen owned by the
// authenticated caller, plus an initial token returned ONCE in the
// response. The caller is responsible for stashing the token where
// the bot will run from (CI env var, daemon config, etc.) — there
// is no recovery path. To rotate, issue a new token via
// (future) tools and revoke the old one.
func (s *Server) handleRegisterBot(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req registerBotRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.RegisterBot(s.store, caller, service.RegisterBotParams{
		Name:     req.Name,
		Username: req.Username,
		Role:     req.Role,
		Label:    req.Label,
	})
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrConflict):
			writeError(w, http.StatusConflict, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, "register bot: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusCreated, resp)
}

// handleListMyBots returns every bot the authenticated caller owns
// (parent_id = caller.id), with each bot's active token labels but
// NOT the token values (those were shown once at registration).
func (s *Server) handleListMyBots(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	resp, err := service.ListMyBots(s.store, caller)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list bots: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type reissueTokenRequest struct {
	Label string `json:"label,omitempty"` // optional label for the fresh token
}

// handleReissueBotToken revokes every active token for the named
// agent and issues a fresh one atomically. Caller must parent the
// agent — ownership enforced in the service layer. The new token
// appears ONCE in the response; treat it the same as a registration
// token.
func (s *Server) handleReissueBotToken(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	username := r.PathValue("username")
	if username == "" {
		writeError(w, http.StatusBadRequest, "username is required")
		return
	}
	var req reissueTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil && !errors.Is(err, io.EOF) {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.ReissueBotToken(s.store, caller, username, req.Label)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		case err == service.ErrNotFound:
			writeError(w, http.StatusNotFound, "agent not found")
		default:
			writeError(w, http.StatusInternalServerError, "reissue: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

type revokeTokenRequest struct {
	Token  string `json:"token,omitempty"`  // revoke by value (caller already holds it)
	TokenID int64 `json:"token_id,omitempty"` // revoke by row id (from list endpoint)
}

// handleRevokeToken marks a token as revoked. The caller must own
// the token: either it's their own (humans rotating their session
// token) or it belongs to a bot they parent. Without that check, a
// member could revoke another member's session — auth bypass via
// denial.
func (s *Server) handleRevokeToken(w http.ResponseWriter, r *http.Request) {
	caller := citizenFromRequest(r)
	if caller == nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req revokeTokenRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid request body")
		return
	}
	resp, err := service.RevokeToken(s.store, caller, req.Token, req.TokenID)
	if err != nil {
		switch {
		case errors.Is(err, service.ErrInvalidArgument):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, service.ErrForbidden):
			writeError(w, http.StatusForbidden, err.Error())
		case err == service.ErrNotFound:
			writeError(w, http.StatusNotFound, "token not found")
		default:
			writeError(w, http.StatusInternalServerError, "revoke: "+err.Error())
		}
		return
	}
	writeJSON(w, http.StatusOK, resp)
}
