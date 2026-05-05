package service

// Project-metadata + workspace-clone helpers shared across tool
// handlers. Every fat-client flow that touches a project's local
// clone goes through these: fetch the project's metadata from the
// coordinator, then open / configure the local workspace clone so
// pulls + pushes target the right ref.

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// FetchProjectMeta reads a project's metadata from the coordinator.
// Used by the client-side project_remote_status / project_sync /
// get_artifact / get_artifact_history / set_project_remote handlers
// that need the project's remote_url to open the local clone.
func (s *FatClient) FetchProjectMeta(ctx context.Context, projectID int64) (remoteURL string, err error) {
	remoteURL, _, err = s.FetchProjectMetaFull(ctx, projectID)
	return
}

// FetchProjectMetaFull is like FetchProjectMeta but also returns
// the human-readable project name for workspace directory naming.
func (s *FatClient) FetchProjectMetaFull(ctx context.Context, projectID int64) (remoteURL, name string, err error) {
	remoteURL, name, _, err = s.FetchProjectMetaExpanded(ctx, projectID)
	return
}

// ResolveProjectWorkspace returns the absolute path to the
// project's local clone, materializing it (clone or init) if
// not yet present. Used by per-process consumers (bot daemon)
// that need to point subprocesses at the right cwd — without
// this, a Handler that spawns `claude -p` (or any subprocess)
// inherits the daemon's cwd, leaking the operator's filesystem
// to the LLM AND letting the bot accidentally write into the
// wrong tree.
//
// Thin wrapper over OpenProject that returns just the
// (path, error) shape the daemon needs, so the bots package
// doesn't have to import workspace just to read WorkDir().
// Path is absolute and stable for the life of the project on
// this machine.
func (s *FatClient) ResolveProjectWorkspace(ctx context.Context, projectID int64) (string, error) {
	proj, _, _, _, err := s.OpenProject(ctx, projectID)
	if err != nil {
		return "", err
	}
	if proj == nil {
		return "", fmt.Errorf("no workspace project for project_id=%d", projectID)
	}
	return proj.WorkDir(), nil
}

// OpenProject fetches project metadata, opens the workspace
// clone, AND wires the project's default_branch into the
// Project so Pull/Push fallback paths target the right ref.
// Every call site that pairs FetchProjectMetaFull +
// workspace.ForProject should use this helper instead.
func (s *FatClient) OpenProject(ctx context.Context, projectID int64) (proj *workspace.Project, remoteURL, projName, defaultBranch string, err error) {
	if s.workspace == nil {
		return nil, "", "", "", fmt.Errorf("no workspace configured")
	}
	remoteURL, projName, defaultBranch, err = s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, "", "", "", err
	}
	proj, err = s.workspace.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, remoteURL, projName, defaultBranch, err
	}
	proj.SetDefaultBranch(defaultBranch)
	return proj, remoteURL, projName, defaultBranch, nil
}

// FetchProjectMetaExpanded returns remote_url + name +
// default_branch. Called from paths that need the branch name
// to configure the workspace (submit / claim / execute) so
// Pull/Push target the right ref.
func (s *FatClient) FetchProjectMetaExpanded(ctx context.Context, projectID int64) (remoteURL, name, defaultBranch string, err error) {
	data, err := s.coord.Get(ctx, fmt.Sprintf("/api/v1/projects/%d", projectID))
	if err != nil {
		return "", "", "", err
	}
	var raw map[string]interface{}
	if err := json.Unmarshal(data, &raw); err != nil {
		return "", "", "", fmt.Errorf("parsing project: %w", err)
	}
	if errMsg, ok := raw["error"].(string); ok {
		return "", "", "", fmt.Errorf("%s", errMsg)
	}
	if v, ok := raw["remote_url"].(string); ok {
		remoteURL = v
	}
	if v, ok := raw["name"].(string); ok {
		name = v
	}
	if v, ok := raw["default_branch"].(string); ok {
		defaultBranch = v
	}
	return remoteURL, name, defaultBranch, nil
}
