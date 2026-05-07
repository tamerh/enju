package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// CommitFiles writes the given files to the worktree, stages
// exactly the listed paths, and creates a commit on the current
// branch with the supplied message and author.
//
// The message is OPAQUE: this layer composes nothing — no
// trailers, no AI-Model line, no Co-Authored-By. Caller (enjugit)
// passes the fully-formed message string.
//
// Authorship is OPAQUE: AuthorName/AuthorEmail are passed
// straight to git Author + Committer fields. When both are empty,
// falls back to a generic "Enju git layer" placeholder so the
// commit is still parseable. Enjugit always passes real values.
//
// Idempotent no-op: when none of the requested files would change
// the worktree (every target path already holds identical bytes),
// no commit is created. Result.NoOp is true and Result.SHA is the
// SHA of HEAD before the call.
//
// Git operations performed:
//   1. Verify req.StagePaths is a subset of Files paths.
//   2. Write each Files entry to the worktree.
//   3. Detect no-op: if no working-tree changes vs HEAD's tree, return.
//   4. wt.Add() each path in StagePaths (NOT AddGlob ".").
//   5. wt.Commit(message, opts).
//
// Worktree state: any → matches HEAD's new tree (StateClean).
//
// Errors: any go-git error wrapped with context.
func (c *Clone) CommitFiles(req CommitRequest) (CommitResult, error) {
	defer c.lock()()
	if len(req.Files) == 0 {
		return CommitResult{}, fmt.Errorf("git: CommitFiles called with empty Files")
	}
	// Validate StagePaths ⊆ Files paths. Catches caller errors
	// before we touch the worktree.
	if len(req.StagePaths) > 0 {
		fileSet := make(map[string]bool, len(req.Files))
		for _, f := range req.Files {
			fileSet[f.RepoRelPath] = true
		}
		for _, p := range req.StagePaths {
			if !fileSet[p] {
				return CommitResult{}, fmt.Errorf("git: StagePath %q not in Files", p)
			}
		}
	} else {
		// Default: stage every file in Files.
		req.StagePaths = make([]string, len(req.Files))
		for i, f := range req.Files {
			req.StagePaths[i] = f.RepoRelPath
		}
	}

	// Write the files. Track whether any actually changed bytes.
	anyChanged := false
	for _, f := range req.Files {
		full := filepath.Join(c.workDir, f.RepoRelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return CommitResult{}, fmt.Errorf("git: mkdir for %s: %w", f.RepoRelPath, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		// Compare existing content; skip write if identical.
		if existing, err := os.ReadFile(full); err == nil && bytesEqual(existing, f.Content) {
			// Permissions might still differ; chmod for safety.
			_ = os.Chmod(full, mode)
			continue
		}
		anyChanged = true
		if err := os.WriteFile(full, f.Content, mode); err != nil {
			return CommitResult{}, fmt.Errorf("git: write %s: %w", f.RepoRelPath, err)
		}
		if err := os.Chmod(full, mode); err != nil {
			return CommitResult{}, fmt.Errorf("git: chmod %s: %w", f.RepoRelPath, err)
		}
	}

	// Note: we previously short-circuited to NoOp here when the
	// loop above wrote nothing — but matching bytes-on-disk does
	// not imply the index already has the file. Untracked files
	// with already-correct content still need wt.Add + commit to
	// be tracked. The wt.Commit call below returns ErrEmptyCommit
	// when staging genuinely produces no change, and we map that
	// to NoOp (see line ~129).
	_ = anyChanged

	wt, err := c.repo.Worktree()
	if err != nil {
		return CommitResult{}, fmt.Errorf("git: worktree handle: %w", err)
	}
	for _, p := range req.StagePaths {
		if _, err := wt.Add(p); err != nil {
			return CommitResult{}, fmt.Errorf("git: stage %s: %w", p, err)
		}
	}

	authorName := req.AuthorName
	authorEmail := req.AuthorEmail
	if authorName == "" && authorEmail == "" {
		authorName = "Enju git layer"
		authorEmail = "enju-git@localhost"
	}
	sig := &object.Signature{
		Name:  authorName,
		Email: authorEmail,
		When:  time.Now(),
	}
	hash, err := wt.Commit(req.Message, &gogit.CommitOptions{
		Author:    sig,
		Committer: sig,
	})
	if err != nil {
		// Detect "nothing to commit" and convert to NoOp result.
		if errors.Is(err, gogit.ErrEmptyCommit) {
			head, herr := c.repo.Head()
			if herr == nil {
				return CommitResult{SHA: head.Hash().String(), NoOp: true}, nil
			}
		}
		return CommitResult{}, fmt.Errorf("git: commit: %w", err)
	}
	return CommitResult{SHA: hash.String()}, nil
}

// bytesEqual is a tiny helper used by CommitFiles' no-op
// detection. Avoids the import-bytes overhead for a one-liner.
func bytesEqual(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
