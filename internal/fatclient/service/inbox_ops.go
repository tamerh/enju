package service

// enju_inbox backing — the inbox projection lives in
// internal/fatclient/inbox, but constructing the deps it needs
// (open project clone, locate live.jsonl, wire the
// ReadFileAtCommit adapter) is workspace orchestration that
// belongs here. Returns ProjectClonePresent=false to let the
// handler render a friendly "no clone yet" message rather than
// surface an error.

import (
	"context"
	"fmt"
	"path/filepath"

	"github.com/enju-ai/enju/internal/fatclient/inbox"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// InboxResult bundles the inbox rows with the project-clone
// presence flag so the handler can distinguish "no waiting
// tasks" from "project clone not yet materialized."
type InboxResult struct {
	Rows                []inbox.InboxRow
	ProjectClonePresent bool
}

// BuildInbox opens the project clone, locates live.jsonl, and
// runs the inbox projection (event replay → parent walk →
// content read → row build). Returns ProjectClonePresent=false
// when the workspace knows the project but no clone has been
// materialized yet — handler surfaces a friendly message
// rather than treating it as an error.
func (s *FatClient) BuildInbox(ctx context.Context, projectID int64, username string) (*InboxResult, error) {
	if s.workspace == nil {
		return nil, fmt.Errorf("workspace not configured")
	}
	projectDir := s.workspace.ProjectDir(projectID)
	if projectDir == "" {
		return &InboxResult{ProjectClonePresent: false}, nil
	}

	remoteURL, projName, _, err := s.FetchProjectMetaExpanded(ctx, projectID)
	if err != nil {
		return nil, err
	}
	proj, err := s.workspace.ForProject(projectID, remoteURL, projName)
	if err != nil {
		return nil, fmt.Errorf("opening project clone: %w", err)
	}

	livePath := filepath.Join(projectDir, "enju", "events", "live.jsonl")
	rows, err := inbox.BuildInbox(livePath, username, &inboxGitDeps{proj: proj})
	if err != nil {
		return nil, err
	}
	return &InboxResult{Rows: rows, ProjectClonePresent: true}, nil
}

// inboxGitDeps adapts a project clone to inbox.Deps. The full
// Deps surface today is just one method (git read at commit) —
// the projection is otherwise self-contained over live.jsonl.
type inboxGitDeps struct {
	proj *workspace.Project
}

func (d *inboxGitDeps) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	return d.proj.ReadFileAtCommit(commitSHA, repoRelPath)
}
