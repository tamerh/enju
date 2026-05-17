package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// BotTokenInfo is one token row exposed for a bot, with revoked
// status surfaced when applicable. Tokens themselves never appear
// on the wire — only IDs and metadata.
type BotTokenInfo struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	IssuedAt  string `json:"issued_at"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// BotInfo is one bot citizen + its tokens.
type BotInfo struct {
	ID         int64          `json:"id"`
	Username   string         `json:"username"`
	Name       string         `json:"name"`
	Role       string         `json:"role"`
	Registered string         `json:"registered"`
	Tokens     []BotTokenInfo `json:"tokens"`
}

// BotListResponse is the wire shape for enju_list_my_agents /
// GET /citizens/me/agents. Wrapped in a {"agents":...} envelope so
// the formatter can disambiguate between empty list and decode
// failure.
type BotListResponse struct {
	Bots []BotInfo `json:"agents"`
}

// ListMyBots returns the bots the caller parents, each with
// their tokens. Caller must be authenticated.
func ListMyBots(s store.CoordinatorStore, caller *store.CitizenRecord) (*BotListResponse, error) {
	bots, err := s.ListBotsByParent(caller.ID)
	if err != nil {
		return nil, err
	}
	out := &BotListResponse{Bots: make([]BotInfo, 0, len(bots))}
	for _, b := range bots {
		tokens, _ := s.ListTokensByCitizen(b.ID)
		tokenInfo := make([]BotTokenInfo, 0, len(tokens))
		for _, t := range tokens {
			info := BotTokenInfo{
				ID:       t.ID,
				Label:    t.Label,
				IssuedAt: t.IssuedAt.Format(time.RFC3339),
			}
			if t.RevokedAt != nil {
				info.RevokedAt = t.RevokedAt.Format(time.RFC3339)
			}
			tokenInfo = append(tokenInfo, info)
		}
		out.Bots = append(out.Bots, BotInfo{
			ID:         b.ID,
			Username:   b.Username,
			Name:       b.Name,
			Role:       b.Role,
			Registered: b.RegisteredAt.Format(time.RFC3339),
			Tokens:     tokenInfo,
		})
	}
	return out, nil
}

