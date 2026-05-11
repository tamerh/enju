package gitv6

import (
	"errors"
	"fmt"
	"strings"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/config"
	"github.com/go-git/go-git/v6/plumbing"
)

// Push pushes the named local branch to origin. Refuses non-FF
// updates with ErrPushNonFF (no force here — use a separate verb
// if force is ever needed). Caller already holds the lock.
//
// Git operations performed:
//   1. Verify branch exists locally.
//   2. repo.Push(refspec="refs/heads/<branch>:refs/heads/<branch>", auth).
//
// Errors:
//   - ErrRefNotFound: branch doesn't exist locally.
//   - ErrPushNonFF: remote ref isn't an ancestor of local tip.
//   - any other: wrapped as "git: push <branch>: ..."
//
// Origin is assumed configured — every project gets one at
// creation (managed bare for path-mode, real URL for remote-mode).
// A genuinely missing origin (manual `git remote rm origin`,
// corrupt restore) surfaces as the underlying gogit error.
func (c *Clone) Push(branch string) error {
	defer c.lock()()
	return c.pushInternal(branch, false)
}

// PushWithVerify pushes then re-reads the remote ref. Returns
// ErrPushVerifyFailed when the remote ref didn't update to
// expectedSHA — most often because a server-side hook rejected
// the commit silently.
//
// Used by enjugit's SubmitTaskResult so silent push failures
// surface to the runner instead of leaving the system thinking
// work was published.
//
// Git operations performed:
//   1. Push (per Push contract).
//   2. ls-remote origin <branch>; compare to expectedSHA.
//
// Errors:
//   - All Push errors.
//   - ErrPushVerifyFailed: push succeeded but remote ref doesn't
//     match expectedSHA. Carries actual local/remote SHAs.
func (c *Clone) PushWithVerify(branch, expectedSHA string) error {
	defer c.lock()()
	if err := c.pushInternal(branch, false); err != nil {
		return err
	}
	return c.verifyRemoteMatches(branch, expectedSHA)
}

// Fetch brings down all remote branches via the full refspec.
// Idempotent; cheap when nothing's new (returns nil for
// NoErrAlreadyUpToDate).
//
// Git operations performed:
//   1. repo.Fetch(refspec="+refs/heads/*:refs/remotes/origin/*", auth).
//
// Errors:
//   - underlying gogit error wrapped (e.g. "remote not found"
//     when origin is genuinely missing — should not happen for
//     healthy projects after Phase 1's auto-bare).
func (c *Clone) Fetch() error {
	defer c.lock()()
	// Pre-fetch sweep: clear any tmp_pack_* left from an
	// interrupted earlier fetch in this session. Same rationale
	// as FetchBranch — the OpenClone-time sweep only catches
	// at-startup orphans, not mid-session ones.
	sweepStaleTempPackFiles(c.workDir, c.logger)
	err := c.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs: []config.RefSpec{
			config.RefSpec("+refs/heads/*:refs/remotes/origin/*"),
		},
		ClientOptions: clientOptionsFor(c.remoteURL),
	})
	if err != nil && !errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		// Sweep again — this fetch may have been the one
		// interrupted, leaving its half-written temp file
		// behind to break the next attempt.
		sweepStaleTempPackFiles(c.workDir, c.logger)
		return fmt.Errorf("git: fetch: %w", err)
	}
	return nil
}

// PushAllRefs pushes every local branch to origin via the
// `refs/heads/*:refs/heads/*` wildcard refspec. Used to seed a
// freshly-pointed remote (enju_set_project_remote) with the
// project's full branch state, including run / topic branches
// that don't yet exist on origin.
//
// Idempotent on no-op (NoErrAlreadyUpToDate). lastPushAt /
// lastPushError state is updated regardless of success or failure
// — the project_remote_status diagnostic reads it.
//
// Git operations performed:
//   1. repo.Push(refspec="refs/heads/*:refs/heads/*", auth).
//
// Errors:
//   - underlying gogit error wrapped as "git: push-all: ...".
func (c *Clone) PushAllRefs(force bool) error {
	defer c.lock()()
	err := c.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec("refs/heads/*:refs/heads/*")},
		Force:      force,
		ClientOptions: clientOptionsFor(c.remoteURL),
	})
	c.lastPushAt = time.Now()
	if err == nil || errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		c.lastPushError = ""
		return nil
	}
	c.lastPushError = err.Error()
	return fmt.Errorf("git: push-all: %w", err)
}

// pushInternal does the actual push. Caller already holds the lock.
// force is hardcoded false in current callers; reserved for a
// future force-push verb if needed.
func (c *Clone) pushInternal(branch string, force bool) error {
	refName := plumbing.NewBranchReferenceName(branch)
	if _, err := c.repo.Reference(refName, false); err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, branch)
	}
	prefix := ""
	if force {
		prefix = "+"
	}
	refspec := fmt.Sprintf("%srefs/heads/%s:refs/heads/%s", prefix, branch, branch)
	err := c.repo.Push(&gogit.PushOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{config.RefSpec(refspec)},
		ClientOptions: clientOptionsFor(c.remoteURL),
	})
	if err == nil || errors.Is(err, gogit.NoErrAlreadyUpToDate) {
		c.lastPushAt = time.Now()
		c.lastPushError = ""
		return nil
	}
	if isNonFastForward(err) {
		c.lastPushError = err.Error()
		return fmt.Errorf("%w: %v", ErrPushNonFF, err)
	}
	c.lastPushError = err.Error()
	return fmt.Errorf("git: push %s: %w", branch, err)
}

// verifyRemoteMatches lists the remote ref and checks that it
// points at expectedSHA. Returns ErrPushVerifyFailed when the
// remote ref's SHA doesn't match.
func (c *Clone) verifyRemoteMatches(branch, expectedSHA string) error {
	rem, err := c.repo.Remote("origin")
	if err != nil {
		return fmt.Errorf("git: lookup origin: %w", err)
	}
	refs, err := rem.List(&gogit.ListOptions{
		ClientOptions: clientOptionsFor(c.remoteURL),
	})
	if err != nil {
		return fmt.Errorf("git: ls-remote: %w", err)
	}
	wantRef := plumbing.NewBranchReferenceName(branch)
	for _, r := range refs {
		if r.Name() == wantRef {
			actual := r.Hash().String()
			if actual == expectedSHA {
				return nil
			}
			return &ErrVerifyFailed{
				Branch:    branch,
				LocalSHA:  expectedSHA,
				RemoteSHA: actual,
			}
		}
	}
	// Remote doesn't have the branch at all → verify failed.
	return &ErrVerifyFailed{
		Branch:    branch,
		LocalSHA:  expectedSHA,
		RemoteSHA: "",
	}
}

// isNonFastForward sniffs go-git's error string for the non-FF
// signature. go-git doesn't expose a typed error for this case,
// so we string-match — but only here, behind our typed wrapper.
// Callers see ErrPushNonFF (typed); they never need to know
// about this string check.
func isNonFastForward(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "non fast-forward") ||
		strings.Contains(msg, "ErrNonFastForwardUpdate")
}
