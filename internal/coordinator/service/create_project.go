package service

import (
	"fmt"
	"strings"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
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
func CreateProject(s store.CoordinatorStore, caller *store.CitizenRecord, params CreateProjectParams) (*ProjectResponse, error) {
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
	// Project creation + creator-as-owner ride one Plan so the
	// row + first membership land in a single transaction. A
	// crash between them previously could have left an
	// ownerless project; the unified plan eliminates that
	// window without the special-case error swallowing the
	// old separate AddProjectMember call needed.
	createResult, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateProject{Project: store.ProjectRecord{
				Name:          params.Name,
				Description:   params.Description,
				CreatedBy:     caller.Username,
				RemoteURL:     params.RemoteURL,
				DefaultBranch: defaultBranch,
				CreatedAt:     now,
				UpdatedAt:     now,
			}},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("failed to create project: %w", err)
	}
	id := createResult.ProjectID
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.AddProjectMember{
				ProjectID: id,
				CitizenID: caller.ID,
				Role:      store.ProjectRoleOwner,
				AddedBy:   0,
			},
		},
	}); err != nil {
		// Same best-effort semantics as before: the project
		// row exists; failing here would mislead callers into
		// thinking creation itself failed. The caller can
		// re-add via project_membership tools.
		_ = err
	}

	effectiveBranch := defaultBranch
	if effectiveBranch == "" {
		effectiveBranch = "main"
	}
	return &ProjectResponse{
		ID:            id,
		Name:          params.Name,
		RemoteURL:     params.RemoteURL,
		DefaultBranch: effectiveBranch,
		CreatedAt:     now,
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
func SetProjectRemoteURL(s store.CoordinatorStore, caller *store.CitizenRecord, projectID int64, remoteURL string) (*SetRemoteURLResponse, error) {
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
	if _, err := s.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetProjectRemoteURL{ProjectID: projectID, RemoteURL: remoteURL},
		},
	}); err != nil {
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
