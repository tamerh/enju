package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// MemberResponse is the wire shape for one project membership
// row. Used by REST + MCP. JSON tags are load-bearing —
// format.ProjectMemberList consumes them.
type MemberResponse struct {
	Username string `json:"username"`
	Name     string `json:"name,omitempty"`
	Role     string `json:"role"`
	AddedAt  string `json:"added_at"`
	AddedBy  string `json:"added_by,omitempty"`
}

// ListProjectMembers returns every member on the project,
// gated on caller membership. Mirrors api.handleListProjectMembers.
func ListProjectMembers(s *store.Store, caller *store.CitizenRecord, projectID int64) ([]MemberResponse, error) {
	if !CanReadProject(s, projectID, caller.ID) {
		return nil, ErrNotMember
	}
	rows, err := s.ListProjectMembers(projectID)
	if err != nil {
		return nil, err
	}
	out := make([]MemberResponse, 0, len(rows))
	for _, m := range rows {
		cz, _ := s.GetCitizen(m.CitizenID)
		username, name := "", ""
		if cz != nil {
			username = cz.Username
			name = cz.Name
		}
		out = append(out, MemberResponse{
			Username: username,
			Name:     name,
			Role:     string(m.Role),
			AddedAt:  m.AddedAt.Format(time.RFC3339),
			AddedBy:  CitizenUsername(s, m.AddedBy),
		})
	}
	return out, nil
}
