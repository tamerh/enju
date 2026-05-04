package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ProjectResponse is the wire shape for one project. Used by
// REST (writeJSON), MCP (json.Marshal → format.ProjectList),
// and future Web UI.
//
// JSON tags are load-bearing — format.ProjectList consumes
// these key names. Renaming any of them silently breaks the
// formatter.
type ProjectResponse struct {
	ID            int64  `json:"id"`
	Name          string `json:"name"`
	Description   string `json:"description,omitempty"`
	RemoteURL     string `json:"remote_url,omitempty"`
	DefaultBranch string `json:"default_branch,omitempty"`
	RunCount      int    `json:"run_count"`
	CreatedAt     string `json:"created_at"`
}

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
		CreatedAt:     p.CreatedAt.Format(time.RFC3339),
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
