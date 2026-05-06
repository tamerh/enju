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

	"github.com/enju-ai/enju/internal/fatclient/inbox"
	"github.com/enju-ai/enju/internal/fatclient/project"
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
	if s.project == nil {
		return nil, fmt.Errorf("workspace not configured")
	}
	projectDir := s.project.ProjectDir(projectID)
	if projectDir == "" {
		return &InboxResult{ProjectClonePresent: false}, nil
	}

	// Read-only: use OpenExisting to resolve the on-disk
	// clone via findProjectDir's slug-then-numeric lookup.
	// Avoids the latent ForProject bug where a coord-side
	// project rename would have ForProject(id, newName)
	// compute a fresh slug path that doesn't exist, fall into
	// the clone-from-remote branch, and create a second
	// clone with a different slug suffix. The inbox
	// projection only reads (live.jsonl + git tree at
	// committed SHAs) — it has no business creating clones.
	proj, err := s.project.OpenExisting(projectID)
	if err != nil {
		if errors.Is(err, project.ErrCloneNotFound) {
			return &InboxResult{ProjectClonePresent: false}, nil
		}
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
	proj *project.Clone
}

func (d *inboxGitDeps) ReadFileAtCommit(commitSHA, repoRelPath string) ([]byte, bool, error) {
	return d.proj.ReadFileAtCommit(commitSHA, repoRelPath)
}
