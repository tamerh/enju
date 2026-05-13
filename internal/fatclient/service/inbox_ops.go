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
	"errors"
	"fmt"
	"path/filepath"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/inbox"
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
	if s.enjugit == nil {
		return nil, fmt.Errorf("workspace not configured")
	}
	projectDir := s.enjugit.ProjectDir(projectID)
	if projectDir == "" {
		return &InboxResult{ProjectClonePresent: false}, nil
	}

	// Read-only path uses OpenView so we never lazy-clone.
	// The inbox projection only reads (live.jsonl + git tree at
	// committed SHAs) — it has no business creating clones.
	view, err := s.enjugit.OpenView(projectID)
	if err != nil {
		if errors.Is(err, enjugit.ErrCloneNotFound) {
			return &InboxResult{ProjectClonePresent: false}, nil
		}
		return nil, fmt.Errorf("opening project clone: %w", err)
	}

	livePath := filepath.Join(projectDir, corelayout.EventsDir, "live.jsonl")
	rows, err := inbox.BuildInbox(livePath, username, &inboxGitDeps{view: view})
	if err != nil {
		return nil, err
	}
	return &InboxResult{Rows: rows, ProjectClonePresent: true}, nil
}

// inboxGitDeps adapts an enjugit View to inbox.Deps. The full
// Deps surface today is just one method (git read at commit) —
// the projection is otherwise self-contained over live.jsonl.
type inboxGitDeps struct {
	view *enjugit.View
}

func (d *inboxGitDeps) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	return d.view.ReadFileAtCommit(commitSHA, repoRelPath)
}
