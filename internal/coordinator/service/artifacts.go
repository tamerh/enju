package service

import (
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// ArtifactResponse is the wire shape for one artifact index
// row. JSON tags are load-bearing — format.ArtifactList reads
// them.
//
// Tracked is a *bool so json.Marshal emits the field
// unconditionally (including the false case). Older clients
// without the field assume tracked=true; explicit-false here
// distinguishes the writes_artifacts: track: false case.
type ArtifactResponse struct {
	Path       string `json:"path"`
	LastWriter string `json:"last_writer,omitempty"`
	LastTaskID string `json:"last_task_id,omitempty"`
	LastRunID  int64  `json:"last_run_id,omitempty"`
	CommitSHA  string `json:"commit_sha,omitempty"`
	Tracked    *bool  `json:"tracked,omitempty"`
	UpdatedAt  string `json:"updated_at"`
}

// ArtifactListParams bundles the optional filter knobs.
// Empty Branch falls back to the project's default branch so
// the common case ("show artifacts on main") just works.
type ArtifactListParams struct {
	ProjectID int64
	Branch    string
	Prefix    string
}

// ListArtifacts returns the project's artifact index rows
// matching the filters. Membership-gated. Branch defaults to
// the project's configured default_branch when empty.
func ListArtifacts(s *store.Store, caller *store.CitizenRecord, p ArtifactListParams) ([]ArtifactResponse, error) {
	if !CanReadProject(s, p.ProjectID, caller.ID) {
		return nil, ErrNotMember
	}
	branch := p.Branch
	if branch == "" {
		if proj, _ := s.GetProject(p.ProjectID); proj != nil {
			branch = proj.DefaultBranch
		}
	}
	rows, err := s.ListArtifactsByProject(p.ProjectID, branch, p.Prefix)
	if err != nil {
		return nil, err
	}
	out := make([]ArtifactResponse, 0, len(rows))
	for _, a := range rows {
		tracked := a.Tracked
		out = append(out, ArtifactResponse{
			Path:       a.Path,
			LastWriter: CitizenUsername(s, a.LastWriter),
			LastTaskID: a.LastTaskID,
			LastRunID:  a.LastRunID,
			CommitSHA:  a.CommitSHA,
			Tracked:    &tracked,
			UpdatedAt:  a.UpdatedAt.Format(time.RFC3339),
		})
	}
	return out, nil
}
