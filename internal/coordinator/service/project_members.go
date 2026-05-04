package service

import (
	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// MemberResponse is an alias for wire.Member — the shared JSON
// shape. Existing call sites stay readable; rename to wire.Member
// on touch.
type MemberResponse = wire.Member

// ListProjectMembers returns every member on the project,
// gated on caller membership. Mirrors api.handleListProjectMembers.
func ListProjectMembers(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64) ([]MemberResponse, error) {
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
			AddedAt:  m.AddedAt,
			AddedBy:  CitizenUsername(s, m.AddedBy),
		})
	}
	return out, nil
}
