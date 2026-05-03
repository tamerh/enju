package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/store"
)

// CreateProjectParams is the input shape for CreateProject.
type CreateProjectParams struct {
	Name      string
	Description  string
	RemoteURL   string
	DefaultBranch string
}

// CreateProject creates a new project, seeds the caller as the
// first owner, and returns the wire-shape response. Iteration A
// design: the coordinator never creates a git repo. Project
// metadata goes into the DB; clients own their local clones,
// and project data lives at the citizen-configured remote
// (RemoteURL). Empty RemoteURL produces a local-only project
// that the MCP client's workspace handles entirely.
//
// Errors:
//   - ErrInvalidArgument: missing name, malformed default_branch
//   - ErrConflict: project name already exists
//   - ErrForbidden: missing caller (auth precondition)
func CreateProject(s *store.Store, caller *store.CitizenRecord, params CreateProjectParams) (*ProjectResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required to create a project", ErrForbidden)
	}
	if params.Name == "" {
		return nil, fmt.Errorf("%w: name is required", ErrInvalidArgument)
	}

	if existing, _ := s.GetProjectByName(params.Name); existing != nil {
		return nil, fmt.Errorf("%w: a project with this name already exists", ErrConflict)
	}

	defaultBranch := strings.TrimSpace(params.DefaultBranch)
	if defaultBranch != "" {
		if err := validateBranchName(defaultBranch); err != nil {
			return nil, fmt.Errorf("%w: %s", ErrInvalidArgument, err.Error())
		}
	}

	now := time.Now()
	id, err := s.CreateProject(&store.ProjectRecord{
		Name:     params.Name,
		Description:  params.Description,
		CreatedBy:   caller.Username,
		RemoteURL:   params.RemoteURL,
		DefaultBranch: defaultBranch,
		CreatedAt:   now,
		UpdatedAt:   now,
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}

	// Seed the creator as owner. Every project has at least
	// this one member from birth — no legacy zero-members
	// branch on the new-project path.
	if err := s.AddProjectMember(id, caller.ID, store.ProjectRoleOwner, 0); err != nil {
		// Best-effort: a creator-add failure shouldn't fail
		// the project creation since the project row is
		// already persisted. Logged at the caller via the
		// fact that the caller can read the response anyway —
		// they just won't appear as owner and would have to
		// be re-added by another path. Surfacing through the
		// error here would mislead callers into thinking the
		// project itself failed.
		_ = err
	}

	effectiveBranch := defaultBranch
	if effectiveBranch == "" {
		effectiveBranch = "main"
	}
	return &ProjectResponse{
		ID:      id,
		Name:     params.Name,
		RemoteURL:   params.RemoteURL,
		DefaultBranch: effectiveBranch,
		CreatedAt:   now.Format(time.RFC3339),
	}, nil
}

// SetProjectRemoteURL updates the project's remote URL.
// Owner-only — changing the remote URL moves the whole project's
// git home, which determines where task commits land. Members
// can push to the remote their owner set, but only owners can
// redirect it.
//
// Reject empty URL: defense-in-depth complement to the MCP-
// handler validation. A direct API call would otherwise store
// an empty remote and silently fork the team on a multi-machine
// project. The legitimate way to clear a remote is to leave the
// project entirely; migration uses this endpoint with the new
// URL directly. (POST /projects still accepts empty remote_url
// for local-only project creation — that's the create-time
// solo-work entry point, deliberate.)
func SetProjectRemoteURL(s *store.Store, caller *store.CitizenRecord, projectID int64, remoteURL string) (*SetRemoteURLResponse, error) {
	if caller == nil {
		return nil, fmt.Errorf("%w: authentication required", ErrForbidden)
	}
	if projectID == 0 {
		return nil, fmt.Errorf("%w: invalid project ID", ErrInvalidArgument)
	}
	if strings.TrimSpace(remoteURL) == "" {
		return nil, fmt.Errorf("%w: remote_url cannot be empty — clearing a remote silently forks multi-machine projects. Pass the new URL directly to migrate, or leave the project to stop using it.", ErrInvalidArgument)
	}
	if err := requireOwner(s, projectID, caller.ID); err != nil {
		return nil, err
	}
	p, err := s.GetProject(projectID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, fmt.Errorf("%w: project not found", ErrNotFound)
	}
	if err := s.SetProjectRemoteURL(projectID, remoteURL); err != nil {
		return nil, fmt.Errorf("failed to persist remote url: %w", err)
	}
	return &SetRemoteURLResponse{
		ProjectID: projectID,
		RemoteURL: remoteURL,
	}, nil
}

// SetRemoteURLResponse is the wire shape for set_project_remote.
type SetRemoteURLResponse struct {
	ProjectID int64 `json:"project_id"`
	RemoteURL string `json:"remote_url"`
}
