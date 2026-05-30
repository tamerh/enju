package service

// Project-settings write methods (default branch, remote URL).
// Same rationale as members_ops.go: the MCP handlers drive these
// coord endpoints with raw apiClient calls plus an existing
// service-side local-materialize step; webui can only import
// service, so the coord PUT gets a home here and is composed
// with the already-public Ensure/Mirror helpers exactly as the
// mcphandler composes them.

import (
	"context"
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// SetProjectDefaultBranch updates the coord-side default branch,
// then materializes it locally so subsequent runs can fork from
// it (mirror of enju_set_project_default_branch). The returned
// warning is non-fatal — the coord update already landed; a
// non-empty warning means only the local materialize step had
// trouble (e.g. couldn't fork a brand-new branch). Owner-only is
// enforced coord-side.
func (s *FatClient) SetProjectDefaultBranch(ctx context.Context, projectID int64, branch string) (warning string, err error) {
	// Capture the OLD default before the update so a brand-new
	// branch can fork from the prior default (matches the
	// mcphandler ordering). Failure here is tolerated —
	// EnsureProjectDefaultBranch no-ops with a warning.
	_, _, oldDefault, _ := s.FetchProjectMetaExpanded(ctx, projectID)

	data, err := s.coord.Put(ctx,
		fmt.Sprintf("/api/v1/projects/%d/default_branch", projectID),
		map[string]string{"branch": branch})
	if err != nil {
		return "", err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	return s.EnsureProjectDefaultBranch(ctx, projectID, branch, oldDefault), nil
}

// SetProjectRemote updates the coord-side remote URL, then
// reconfigures the local clone's origin and seeds it (mirror of
// enju_set_project_remote). Empty remote_url is refused: with
// the scanner's refs/heads fallback it no longer breaks the
// caller's own machine, but on a multi-machine project clearing
// the remote silently bifurcates the team. The returned warning
// is the non-fatal push-seed result from the local mirror step.
func (s *FatClient) SetProjectRemote(ctx context.Context, projectID int64, remoteURL string) (warning string, err error) {
	if strings.TrimSpace(remoteURL) == "" {
		return "", fmt.Errorf("remote_url cannot be empty — clearing a project's remote breaks async reconciliation. " +
			"To migrate, pass the new URL directly; to stop using this project on this machine, leave the project")
	}
	data, err := s.coord.Put(ctx,
		fmt.Sprintf("/api/v1/projects/%d/remote", projectID),
		map[string]string{"remote_url": remoteURL})
	if err != nil {
		return "", err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return "", fmt.Errorf("%s", msg)
	}
	return s.MirrorRemoteAfterSet(projectID, remoteURL), nil
}

// SetProjectPushTopicBranches flips the per-project lever that
// decides whether per-task topic branches are pushed to origin.
// The multi-citizen default is true (push — siblings see each
// other's WIP, async reconcile, cross-machine review). Solo
// bulk-data pipelines flip it to false to keep origin's ref list
// clean at scale (100K runs × N topics → unusable `git
// branch -r`); local-only projects (no remote) are unaffected
// either way. Owner-only is enforced coord-side; the lever is
// read from coord at task-execute time and snapshotted into the
// wrap-spec so the in-flight job's intent survives a mid-run
// flip.
func (s *FatClient) SetProjectPushTopicBranches(ctx context.Context, projectID int64, push bool) error {
	data, err := s.coord.Put(ctx,
		fmt.Sprintf("/api/v1/projects/%d/push_topic_branches", projectID),
		map[string]bool{"push": push})
	if err != nil {
		return err
	}
	if msg := coord.ExtractError(data); msg != "" {
		return fmt.Errorf("%s", msg)
	}
	return nil
}
