package gitcli

// push.go — Phase 5 network operations: fetch, pull, push,
// push-with-verify, push-all-refs, rebase-on-remote.
//
// Auth: nothing to do at this layer. The system `git` binary
// picks up the operator's SSH agent / credential helper config
// exactly as it does at the shell. The `clientOptionsFor` / SSH
// auth plumbing that gitv5/v6 needed disappears entirely.
//
// Timeouts: all verbs here use runOpts{network: true} so
// runGit applies the network timeout (5min) instead of the
// local-op timeout (60s). Operators on slow links get the
// behavior they expect from command-line git.

import (
	"fmt"
	"strings"
	"time"
)

// Fetch brings down all remote branches via the full refspec.
// Idempotent — git fetch is quiet when nothing's new.
func (c *Clone) Fetch() error {
	defer c.lock()()
	if _, err := runGit(c.workDir,
		[]string{"fetch", "origin", "+refs/heads/*:refs/remotes/origin/*"},
		runOpts{network: true}); err != nil {
		return fmt.Errorf("git: fetch: %w", err)
	}
	return nil
}

// FetchBranch updates refs/remotes/origin/<branch> from the
// remote without touching the worktree or local branch refs.
// No-op when the branch doesn't exist on origin (parity with
// gitv6 — used by scan path so a brand-new branch doesn't
// surface as a fetch failure).
func (c *Clone) FetchBranch(branch string) error {
	defer c.lock()()
	if branch == "" {
		return fmt.Errorf("git: FetchBranch: branch required (caller should resolve to default)")
	}
	// ls-remote pre-check matches gitv6's "absent → no-op"
	// semantics. Without this, `git fetch origin <refspec>`
	// against a remote that doesn't carry the branch errors
	// with "couldn't find remote ref ..." — gitv6 callers
	// expect nil for this case.
	remoteSHA, err := c.remoteBranchHashLocked(branch)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	refspec := "+refs/heads/" + branch + ":refs/remotes/origin/" + branch
	if _, err := runGit(c.workDir, []string{"fetch", "origin", refspec}, runOpts{network: true}); err != nil {
		return fmt.Errorf("git: fetch branch %s: %w", branch, err)
	}
	return nil
}

// PullBranch fetches the named branch from origin and FF-updates
// the local branch ref. Distinct from FetchBranch (origin
// tracking only) — Pull also updates refs/heads/<branch> and
// the worktree (when HEAD is on the target branch).
//
// No-op when origin doesn't carry the branch (brand-new branch
// yet to be pushed). FF-only — diverged histories surface as
// errors so callers don't silently lose local commits.
func (c *Clone) PullBranch(branch string) error {
	defer c.lock()()
	if branch == "" {
		return fmt.Errorf("git: PullBranch: branch required (caller should resolve to default)")
	}
	remoteSHA, err := c.remoteBranchHashLocked(branch)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	// Cheap HEAD lookup decides whether worktree mutation is
	// part of this op. We bypass the lock-acquiring Head()
	// because we already hold the lock.
	_, curBranch, _ := c.headLocked()
	if curBranch == branch {
		// Atomic fetch + FF-merge into current branch + worktree.
		// --ff-only means diverged histories error out instead of
		// generating a merge commit.
		if _, err := runGit(c.workDir, []string{"pull", "--ff-only", "origin", branch}, runOpts{network: true}); err != nil {
			return fmt.Errorf("git: pull branch %s: %w", branch, err)
		}
		return nil
	}
	// Not on the target branch — update only the local ref.
	// `git fetch origin <b>:<b>` is FF-only by default (no `+`
	// prefix), so diverged histories error.
	refspec := branch + ":" + branch
	if _, err := runGit(c.workDir, []string{"fetch", "origin", refspec}, runOpts{network: true}); err != nil {
		return fmt.Errorf("git: pull branch %s (fetch-only): %w", branch, err)
	}
	return nil
}

// Push pushes the named local branch to origin. Errors:
//   - ErrRefNotFound: branch doesn't exist locally.
//   - ErrPushNonFF: remote ref isn't an ancestor of local tip.
//   - any other: wrapped as "git: push <branch>: ..."
//
// Updates lastPushAt / lastPushError regardless of outcome so
// the diagnostic surface reflects every attempt.
func (c *Clone) Push(branch string) error {
	defer c.lock()()
	return c.pushInternalLocked(branch, false)
}

// PushWithVerify pushes then re-reads the remote ref to confirm
// it actually moved. Catches the silent-hook-rejection case
// where the push exit code is success but the server-side ref
// didn't update.
//
// Errors:
//   - All Push errors.
//   - ErrPushVerifyFailed (with ErrVerifyFailed details): push
//     succeeded but remote ref doesn't match expectedSHA.
func (c *Clone) PushWithVerify(branch, expectedSHA string) error {
	defer c.lock()()
	if err := c.pushInternalLocked(branch, false); err != nil {
		return err
	}
	return c.verifyRemoteMatchesLocked(branch, expectedSHA)
}

// PushAllRefs pushes every local branch to origin via the
// refs/heads/*:refs/heads/* wildcard refspec. Used to seed a
// freshly-pointed remote (set_project_remote) with the
// project's full branch state, including run/topic branches
// that don't yet exist on origin.
//
// Updates lastPushAt / lastPushError on every call (success or
// failure) — the diagnostic uses this for project_remote_status.
func (c *Clone) PushAllRefs(force bool) error {
	defer c.lock()()
	refspec := "refs/heads/*:refs/heads/*"
	args := []string{"push"}
	if force {
		args = append(args, "--force")
	}
	args = append(args, "origin", refspec)
	_, err := runGit(c.workDir, args, runOpts{network: true})
	c.lastPushAt = time.Now()
	if err == nil {
		c.lastPushError = ""
		return nil
	}
	c.lastPushError = err.Error()
	return fmt.Errorf("git: push-all: %w", err)
}

// RebaseOnRemote runs `git pull --rebase --autostash origin
// <branch>` so divergent histories are merged without
// discarding local commits.
//
// --autostash protects against the edge case where the caller
// left uncommitted changes in the worktree — they get stashed
// and re-applied around the rebase so the caller never gets
// surprised by a dirty-tree rejection.
//
// Same implementation as gitv6: this verb already shelled out
// to git in that backend (go-git's rebase support was too
// limited for divergent-history replays), so the migration is
// trivial here.
func (c *Clone) RebaseOnRemote(branch string) error {
	defer c.lock()()
	if branch == "" {
		return fmt.Errorf("git: rebase-on-remote: branch is required")
	}
	if _, err := runGit(c.workDir,
		[]string{"pull", "--rebase", "--autostash", "origin", branch},
		runOpts{network: true}); err != nil {
		return fmt.Errorf("git pull --rebase origin %s: %w", branch, err)
	}
	return nil
}

// --- internal helpers ---

// pushInternalLocked is the shared push body for Push and
// PushWithVerify. Caller already holds c.lock.
func (c *Clone) pushInternalLocked(branch string, force bool) error {
	if branch == "" {
		return fmt.Errorf("git: push: branch required")
	}
	// Validate local branch exists. Without this, git would
	// error with a less-specific stderr that doesn't map to
	// our typed ErrRefNotFound cleanly.
	if _, err := c.localRefHashLocked(branch); err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, branch)
	}
	prefix := ""
	if force {
		prefix = "+"
	}
	refspec := prefix + "refs/heads/" + branch + ":refs/heads/" + branch
	_, err := runGit(c.workDir, []string{"push", "origin", refspec}, runOpts{network: true})
	c.lastPushAt = time.Now()
	if err == nil {
		c.lastPushError = ""
		return nil
	}
	c.lastPushError = err.Error()
	// classifyStderr maps non-FF stderr to ErrPushNonFF, so
	// the wrapping below preserves errors.Is(ErrPushNonFF).
	return fmt.Errorf("git: push %s: %w", branch, err)
}

// verifyRemoteMatchesLocked lists the remote ref and checks that
// it points at expectedSHA. Returns ErrPushVerifyFailed (with
// ErrVerifyFailed details) when the remote ref's SHA doesn't
// match — most often because a server-side hook rejected the
// commit silently after our local push step claimed success.
func (c *Clone) verifyRemoteMatchesLocked(branch, expectedSHA string) error {
	out, err := runGit(c.workDir, []string{"ls-remote", "origin", "refs/heads/" + branch}, runOpts{network: true})
	if err != nil {
		return fmt.Errorf("git: verify ls-remote %s: %w", branch, err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		// Remote doesn't have the branch at all → verify failed.
		return &ErrVerifyFailed{
			Branch:    branch,
			LocalSHA:  expectedSHA,
			RemoteSHA: "",
		}
	}
	actual, _, _ := strings.Cut(line, "\t")
	if actual == expectedSHA {
		return nil
	}
	return &ErrVerifyFailed{
		Branch:    branch,
		LocalSHA:  expectedSHA,
		RemoteSHA: actual,
	}
}

// remoteBranchHashLocked is the lock-internal twin of the public
// RemoteBranchHash. Verbs holding c.lock can't call the public
// method (would deadlock on a non-reentrant mutex without the
// goroutine-id reentry fast-path being involved on a different
// call stack). Use this instead.
func (c *Clone) remoteBranchHashLocked(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("git: remoteBranchHashLocked: branch required")
	}
	out, err := runGit(c.workDir, []string{"ls-remote", "origin", "refs/heads/" + branch}, runOpts{network: true})
	if err != nil {
		return "", fmt.Errorf("git: ls-remote: %w", err)
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return "", nil
	}
	sha, _, _ := strings.Cut(line, "\t")
	return sha, nil
}

// headLocked is the lock-internal twin of Head. Same rationale as
// remoteBranchHashLocked: callers already holding the lock need
// a non-recursive variant.
func (c *Clone) headLocked() (sha, branch string, err error) {
	out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "HEAD"}, runOpts{})
	if err != nil {
		return "", "", fmt.Errorf("%w: HEAD: %v", ErrRefNotFound, err)
	}
	sha = strings.TrimSpace(string(out))
	bOut, bErr := runGit(c.workDir, []string{"symbolic-ref", "--quiet", "--short", "HEAD"}, runOpts{})
	if bErr == nil {
		branch = strings.TrimSpace(string(bOut))
	}
	return sha, branch, nil
}
