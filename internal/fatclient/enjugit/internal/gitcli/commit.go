package gitcli

// commit.go — CommitFiles (Phase 6). The "normal" commit path:
// write files to worktree, stage, commit. PlumbingCommit (no
// HEAD/index/worktree mutation, parallel-safe) lives in
// plumbing.go.

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// CommitFiles writes the given files to the worktree, stages
// exactly the listed paths, and creates a commit on the current
// branch with the supplied message and author.
//
// The message is OPAQUE: no trailers, no AI-Model line, no
// Co-Authored-By composition at this layer. Caller (enjugit)
// passes the fully-formed string.
//
// Idempotent no-op: when staging produces no index-vs-HEAD
// change (every target path already holds identical bytes),
// no commit is created. Result.NoOp is true and Result.SHA is
// the SHA of HEAD before the call. Detected via
// `git diff --cached --quiet` after staging — cleaner than
// trying to predict no-op from pre-write bytes-equal checks
// (which miss the untracked-but-correct case gitv6 had to
// handle separately).
//
// Worktree state: any → matches new commit's tree (StateClean).
func (c *Clone) CommitFiles(req CommitRequest) (CommitResult, error) {
	defer c.lock()()
	if len(req.Files) == 0 {
		return CommitResult{}, fmt.Errorf("git: CommitFiles called with empty Files")
	}
	// Validate StagePaths ⊆ Files paths. Catches caller errors
	// before any worktree mutation.
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

	// Write files. Skip the actual disk write when existing
	// content already matches — preserves mtime + skips spurious
	// inode churn for tests / file watchers that care.
	for _, f := range req.Files {
		full := filepath.Join(c.workDir, f.RepoRelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			return CommitResult{}, fmt.Errorf("git: mkdir for %s: %w", f.RepoRelPath, err)
		}
		mode := f.Mode
		if mode == 0 {
			mode = 0o644
		}
		if existing, err := os.ReadFile(full); err == nil && bytes.Equal(existing, f.Content) {
			// Permissions might still differ; chmod for safety.
			_ = os.Chmod(full, mode)
			continue
		}
		if err := os.WriteFile(full, f.Content, mode); err != nil {
			return CommitResult{}, fmt.Errorf("git: write %s: %w", f.RepoRelPath, err)
		}
		if err := os.Chmod(full, mode); err != nil {
			return CommitResult{}, fmt.Errorf("git: chmod %s: %w", f.RepoRelPath, err)
		}
	}

	// Stage the explicit paths. `-f` (force): StagePaths is an
	// enju-decided allowlist (we never `git add .`), and enju's own
	// managed result files live under .enju/runs/<run>/<task>/ —
	// which every project's .gitignore intentionally ignores. A
	// plain `git add --` of an explicitly-named ignored path fails
	// the whole commit (a review task, whose only output is
	// .enju/.../result.md, can't commit at all). The project's
	// .gitignore must not veto enju's own explicit commit set.
	// Args list ends with `--` then paths so git can't mistake
	// paths for refs.
	addArgs := append([]string{"add", "-f", "--"}, req.StagePaths...)
	if _, err := runGit(c.workDir, addArgs, runOpts{}); err != nil {
		return CommitResult{}, fmt.Errorf("git: stage: %w", err)
	}

	// No-op detection: if the index now matches HEAD exactly,
	// there's nothing to commit. `git diff --cached --quiet`
	// exits 0 = no diff, 1 = diff present. We DO want to commit
	// if the index has anything new (including newly-tracked
	// files), so this check captures that intent.
	noopErr := error(nil)
	if _, err := runGit(c.workDir, []string{"diff", "--cached", "--quiet"}, runOpts{}); err == nil {
		// Exit 0 → no staged changes → NoOp.
		headSHA, hErr := c.headSHALocked()
		if hErr != nil {
			return CommitResult{}, fmt.Errorf("git: read HEAD for NoOp result: %w", hErr)
		}
		return CommitResult{SHA: headSHA, NoOp: true}, nil
	} else {
		_ = noopErr // diff --cached --quiet exit 1 = normal "has changes" path
	}

	// Build the commit. Author + committer come from env vars so
	// the caller's identity is honored without touching git
	// config. When name/email both empty, use a placeholder so
	// the commit still parses.
	authorName := req.AuthorName
	authorEmail := req.AuthorEmail
	if authorName == "" && authorEmail == "" {
		authorName = "Enju git layer"
		authorEmail = "enju-git@localhost"
	}
	when := time.Now()
	env := authorEnvVars(authorName, authorEmail, when)

	commitOut, err := runGit(c.workDir, []string{"commit", "-m", req.Message}, runOpts{extraEnv: env})
	if err != nil {
		return CommitResult{}, fmt.Errorf("git: commit: %w", err)
	}
	_ = commitOut // we discard commit's chatty stdout; the SHA comes from rev-parse below

	// Read back the new HEAD SHA.
	headSHA, err := c.headSHALocked()
	if err != nil {
		return CommitResult{}, fmt.Errorf("git: read post-commit HEAD: %w", err)
	}
	return CommitResult{SHA: headSHA}, nil
}

// headSHALocked is a tiny twin of Head() that returns only the
// commit SHA, for callers already holding the lock.
func (c *Clone) headSHALocked() (string, error) {
	out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "HEAD"}, runOpts{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// authorEnvVars builds the GIT_AUTHOR_* / GIT_COMMITTER_* env
// var pairs for a commit. Used by both CommitFiles (via `git
// commit`) and PlumbingCommit (via `git commit-tree`) so the
// authorship convention is consistent.
//
// Timestamp format is git's "<unix> <±HHMM>" — using time.Format
// with the right layout produces what git wants.
func authorEnvVars(name, email string, when time.Time) []string {
	if name == "" {
		name = "Enju git layer"
	}
	if email == "" {
		email = "enju-git@localhost"
	}
	stamp := strconv.FormatInt(when.Unix(), 10) + " " + when.Format("-0700")
	return []string{
		"GIT_AUTHOR_NAME=" + name,
		"GIT_AUTHOR_EMAIL=" + email,
		"GIT_AUTHOR_DATE=" + stamp,
		"GIT_COMMITTER_NAME=" + name,
		"GIT_COMMITTER_EMAIL=" + email,
		"GIT_COMMITTER_DATE=" + stamp,
	}
}
