package service

// Agent identity/roster methods. Agents are tenant-scoped
// citizens created under the calling user (POST/GET
// /api/v1/citizens/me/agents) — the MCP handlers drive these
// with raw apiClient coord calls; webui only imports service,
// so the coord calls get a thin home here. 1:1 with the coord
// HTTP surface — no extra policy; the coordinator owns
// ownership/tenant scoping.
//
// Scope note: this is identity only (register + list). Agent
// process lifecycle (start/stop/status/logs) is bots.Supervisor
// — process-local to whichever process launched the agents, so
// it stays a CLI concern, not a service/webui surface.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// RegisterAgentParams are the new-agent fields. Name is
// required; an empty Username triggers slug-from-name on the
// coord; empty Role defaults to "citizen"; Label retags the
// auto-issued initial token.
type RegisterAgentParams struct {
	Name     string
	Username string
	Role     string
	Label    string
}

// RegisterAgentResult mirrors the coord RegisterBotResponse.
// Token is shown EXACTLY ONCE — there is no recovery path, so
// the UI must surface it immediately and unmistakably.
type RegisterAgentResult struct {
	ID         int64  `json:"id"`
	Username   string `json:"username"`
	Name       string `json:"name"`
	ParentName string `json:"parent_name"`
	Token      string `json:"token"`
	Label      string `json:"label,omitempty"`
	Warning    string `json:"warning"`
}

// AgentToken is one issued token for an agent (the roster shows
// these so the owner can see/rotate credentials).
type AgentToken struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	IssuedAt  string `json:"issued_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// AgentSummary is one row of the caller's agent roster.
type AgentSummary struct {
	ID         int64        `json:"id"`
	Username   string       `json:"username"`
	Name       string       `json:"name"`
	Role       string       `json:"role"`
	Registered string       `json:"registered"`
	Tokens     []AgentToken `json:"tokens"`
}

// RegisterAgent creates an agent citizen owned by the caller
// (mirror of enju_register_agent). The coordinator resolves the
// tenant from the caller and rejects an ownerless agent. The
// returned Token is one-time — caller must show it now.
func (s *FatClient) RegisterAgent(ctx context.Context, p RegisterAgentParams) (*RegisterAgentResult, error) {
	if p.Name == "" {
		return nil, fmt.Errorf("name is required")
	}
	body := map[string]string{"name": p.Name}
	if p.Username != "" {
		body["username"] = p.Username
	}
	if p.Role != "" {
		body["role"] = p.Role
	}
	if p.Label != "" {
		body["label"] = p.Label
	}
	data, err := s.coord.Post(ctx, "/api/v1/citizens/me/agents", body)
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var out RegisterAgentResult
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, fmt.Errorf("decode register-agent response: %w", err)
	}
	return &out, nil
}

// ListMyAgents returns the agents the caller owns, each with
// their tokens (mirror of enju_list_my_agents). The coord wraps
// the list in a {"agents":[...]} envelope.
func (s *FatClient) ListMyAgents(ctx context.Context) ([]AgentSummary, error) {
	data, err := s.coord.Get(ctx, "/api/v1/citizens/me/agents")
	if err != nil {
		return nil, err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return nil, fmt.Errorf("%s", msg)
	}
	var env struct {
		Agents []AgentSummary `json:"agents"`
	}
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("decode agents list: %w", err)
	}
	return env.Agents, nil
}
