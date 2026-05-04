package service

import (
	"github.com/enju-ai/enju/internal/common/wire"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ProjectResponse is an alias for wire.Project — the JSON shape
// shared with the fat-client. The alias keeps existing
// coord-side call sites readable; rename to wire.Project on
// touch.
type ProjectResponse = wire.Project

// ToProjectResponse builds the wire shape from a store record
// + a pre-computed run count. Exported so other service
// functions (single-project reads, dashboards) can produce
// the same shape from a single record.
func ToProjectResponse(p store.ProjectRecord, runCount int) ProjectResponse {
	return ProjectResponse{
		ID:            p.ID,
		Name:          p.Name,
		Description:   p.Description,
		RemoteURL:     p.RemoteURL,
		DefaultBranch: p.DefaultBranch,
		RunCount:      runCount,
		CreatedAt:     p.CreatedAt,
	}
}

// ListProjects returns the projects the caller is a member of,
// each with a pre-computed run count. Membership is the gate —
// non-members never see the project at all.
//
// Caller must be non-nil (the transport layer enforces auth).
func ListProjects(s store.CoordinatorStore, caller *store.CitizenRecord) ([]ProjectResponse, error) {
	projects, err := s.ListProjectsForCitizen(caller.ID)
	if err != nil {
		return nil, err
	}
	out := make([]ProjectResponse, 0, len(projects))
	for _, p := range projects {
		runs, _ := s.ListRunsByProject(p.ID)
		out = append(out, ToProjectResponse(p, len(runs)))
	}
	return out, nil
}
