package gitcli

// worktree.go — Phase 4 worktree mutations. All acquire the
// project lock. The composite verb (CheckoutBranchFrom) carries
// the stale-ref guard and fork-from-base logic the topic-branch
// flow depends on.
//
// CLI vs gitv6: most of these collapse dramatically. The v6
// implementations had to manually detach HEAD, Reset, and
// re-attach to work around two go-git ordering quirks
// (setHEADToBranch-before-Reset, and Reset-clobbers-branch-ref).
// Real git's `git checkout -f <branch>` handles all of this
// correctly internally; we just shell out and trust the
// reference implementation.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Checkout switches HEAD to an existing local branch and updates
// the worktree to match. Force is implied: tracked-file
// modifications are overwritten. Untracked files survive
// (native git behavior — same as v6's PR #1903 buy).
//
// Errors:
//   - ErrRefNotFound: branch doesn't exist locally.
func (c *Clone) Checkout(branch string) error {
	defer c.lock()()
	if branch == "" {
		return fmt.Errorf("git: Checkout: branch required")
	}
	// Validate ref exists so the caller gets the typed error
	// they expect. Without this, `git checkout` would emit
	// "pathspec 'X' did not match any file(s)" which our
	// classifier maps to ErrRefNotFound — so this is technically
	// redundant. But explicit-check-first is cheaper than the
	// failed-checkout's broader error surface, and surfaces
	// problems before any worktree mutation happens.
	if _, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "refs/heads/" + branch}, runOpts{}); err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, branch)
	}
	if _, err := runGit(c.workDir, []string{"checkout", "-f", branch}, runOpts{}); err != nil {
		return fmt.Errorf("git: checkout %s: %w", branch, err)
	}
	return nil
}

// CheckoutCommit puts HEAD in detached state on the given commit
// and updates the worktree. No branch ref is created or modified.
//
// Errors:
//   - ErrCommitNotFound: sha isn't a real commit.
func (c *Clone) CheckoutCommit(sha string) error {
	defer c.lock()()
	if sha == "" {
		return fmt.Errorf("git: CheckoutCommit: sha required")
	}
	if _, err := runGit(c.workDir, []string{"cat-file", "-e", sha + "^{commit}"}, runOpts{}); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
	}
	// --detach makes the intent explicit; without it `git
	// checkout <sha>` would also detach but with a warning
	// banner about "detached HEAD."
	if _, err := runGit(c.workDir, []string{"checkout", "-f", "--detach", sha}, runOpts{}); err != nil {
		return fmt.Errorf("git: checkout --detach %s: %w", sha, err)
	}
	return nil
}

// CheckoutBranch is the reconcile-path entry point: switches the
// worktree to the named branch. SOFT (no -f) so dirty-tracked
// files don't get overwritten — gitv6's contract for this verb
// (preserves chmod-edited scripts during pre-claim reconcile,
// etc.).
//
// branch == "" is a no-op (matches PullBranchWithReconcile
// semantics so callers don't special-case).
//
// Auto-track from origin: if refs/heads/<branch> doesn't exist
// but refs/remotes/origin/<branch> does, plant the local ref at
// origin's tip before checking out. Mirrors the gitv6
// resolveLocalOrPlantFromOrigin path.
//
// Dirty-tree handling: gitv6's wt.Checkout refused on a
// modified tracked file; we replicate that explicitly by
// checking State() and no-oping when dirty. Without this, CLI
// would proceed on dirty-tree cases that gitv6 refused, and
// the chmod-preservation property would regress.
func (c *Clone) CheckoutBranch(branch string) error {
	defer c.lock()()
	if branch == "" {
		return nil
	}
	if err := c.ensureLocalOrPlantFromOriginLocked(branch); err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, branch)
	}
	// Same-branch fast path. State checks below are unnecessary
	// when nothing needs to change.
	if _, curBranch, err := c.Head(); err == nil && curBranch == branch {
		return nil
	}
	// Refuse-on-dirty mirror. State() runs symbolic-ref + status
	// internally; both are cheap.
	switch c.State() {
	case StateDirtyTracked, StateDirtyUntracked:
		// gitv6 semantics: soft checkout silently no-ops on
		// dirty so the worktree's in-progress edits / chmod
		// bits survive the reconcile attempt. Callers needing
		// destructive switch use Checkout (force) instead.
		return nil
	}
	if _, err := runGit(c.workDir, []string{"checkout", branch}, runOpts{}); err != nil {
		return fmt.Errorf("git: checkout %s: %w", branch, err)
	}
	return nil
}

// ResetClean hard-resets the worktree to HEAD and removes any
// untracked + ignored files (matches gitv6's filepath.Walk that
// deleted everything not in the index). Skips .bare.git /
// .clone so the managed-bare + adjacent-clone layout survives
// a clean.
//
// Errors:
//   - ErrRefNotFound: HEAD is unresolvable (empty repo).
func (c *Clone) ResetClean() error {
	defer c.lock()()
	// HEAD pre-check for the typed error parity.
	if _, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "HEAD"}, runOpts{}); err != nil {
		return fmt.Errorf("%w: HEAD: %v", ErrRefNotFound, err)
	}
	if _, err := runGit(c.workDir, []string{"reset", "--hard", "HEAD"}, runOpts{}); err != nil {
		return fmt.Errorf("git: hard reset: %w", err)
	}
	// -f force, -d include untracked dirs, -x include ignored
	// files (matches gitv6's index-vs-walk-everything-else
	// behavior). -e excludes our managed-infrastructure paths.
	if _, err := runGit(c.workDir,
		[]string{"clean", "-fdx", "-e", ".bare.git", "-e", ".clone"},
		runOpts{}); err != nil {
		return fmt.Errorf("git: clean: %w", err)
	}
	return nil
}

// SyncIndexToHead updates the index to match HEAD's tree without
// touching the worktree (MixedReset semantics).
//
// Errors:
//   - ErrRefNotFound: HEAD is unresolvable.
func (c *Clone) SyncIndexToHead() error {
	defer c.lock()()
	if _, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "HEAD"}, runOpts{}); err != nil {
		return fmt.Errorf("%w: HEAD: %v", ErrRefNotFound, err)
	}
	// --mixed is the default; spelled explicit for the reader.
	if _, err := runGit(c.workDir, []string{"reset", "--mixed", "HEAD"}, runOpts{}); err != nil {
		return fmt.Errorf("git: mixed reset: %w", err)
	}
	return nil
}

// RemoveFiles deletes the given paths from the worktree.
// Idempotent: a path that doesn't exist is silently skipped.
//
// Implementation matches gitv6 exactly — uses os.Remove
// directly rather than `git rm`. The verb's contract is "drop
// worktree files," NOT "stage deletions." git rm would also
// stage them, which is a different semantic the caller doesn't
// want.
func (c *Clone) RemoveFiles(paths []string) error {
	defer c.lock()()
	for _, p := range paths {
		full := filepath.Join(c.workDir, p)
		if err := os.Remove(full); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("git: remove %s: %w", p, err)
		}
	}
	return nil
}

// CheckoutBranchFrom switches the working tree to `branch`,
// creating it locally if it doesn't exist. When `baseBranch` is
// non-empty AND `branch` doesn't yet exist (or its existing
// local ref is stale w.r.t. baseBranch), the new branch is
// forked from baseBranch's tip.
//
// When `baseBranch` is empty, behavior degrades to "checkout
// branch": the project's default base (refs/heads/<defaultBranch>,
// then origin/<defaultBranch>, then origin/HEAD, then the repo
// root commit) is used as the fork point.
//
// Stale-ref guard: a previous attempt may have created
// refs/heads/<branch> at the wrong base (e.g. crashed before
// committing). When baseBranch is given, we verify its tip is
// an ancestor of the existing local ref; if not, the local ref
// is stale and gets recreated. This is the production-bug fix
// behind the review_domain/iter-2 forking-from-seed regression.
//
// When baseBranch is empty (upstream-tracking semantics) and
// the existing local ref disagrees with origin/<target>, the
// local ref is reset to origin's tip. Same stale-recovery
// principle, just keyed on the upstream rather than an explicit
// base.
//
// Caller MUST hold the project lock (this method itself
// acquires it; nested calls from within WithLock see the
// reentrant fast-path).
func (c *Clone) CheckoutBranchFrom(branch, baseBranch, defaultBranch string) error {
	defer c.lock()()

	target := branch
	if target == "" {
		target = defaultBranch
		if target == "" {
			target = "main"
		}
	}

	// Same-branch fast path.
	if _, curBranch, err := c.Head(); err == nil && curBranch == target {
		return nil
	}

	existing, existingErr := c.localRefHashLocked(target)
	branchExists := existingErr == nil && existing != ""

	if branchExists {
		// Stale-ref handling depends on whether the caller
		// supplied baseBranch (topic-branch flow) or not
		// (upstream-tracking flow).
		if baseBranch == "" {
			// Upstream-tracking: if origin disagrees with
			// existing local, the local is stale — reset.
			if originHash, ok := c.resolveOriginRefHashLocked(target); ok && originHash != existing {
				if c.logger != nil {
					c.logger.Warn("upstream-tracking ref disagrees with origin; resetting",
						"branch", target,
						"stale_hash", existing,
						"origin_hash", originHash)
				}
				if _, err := runGit(c.workDir, []string{"update-ref", "-d", "refs/heads/" + target}, runOpts{}); err != nil {
					return fmt.Errorf("removing stale upstream-tracking ref %s: %w", target, err)
				}
				// Fall through to create-new path.
			} else {
				// Origin agrees (or doesn't exist) — honor the
				// existing ref.
				return c.forceCheckoutBranchLocked(target)
			}
		} else {
			// Explicit baseBranch — verify baseBranch tip is an
			// ancestor of existing.
			baseHash, ok := c.resolveBaseBranchHashLocked(baseBranch)
			if !ok {
				// baseBranch unresolvable — honor existing
				// (don't make things worse on a transient
				// lookup miss).
				return c.forceCheckoutBranchLocked(target)
			}
			isAnc, _ := c.IsAncestor(baseHash, existing)
			if isAnc {
				return c.forceCheckoutBranchLocked(target)
			}
			// Stale: reset and recreate at baseBranch.
			if c.logger != nil {
				c.logger.Warn("stale topic-branch ref detected; resetting to baseBranch tip",
					"branch", target, "stale_hash", existing,
					"base_branch", baseBranch, "base_hash", baseHash)
			}
			if _, err := runGit(c.workDir, []string{"update-ref", "-d", "refs/heads/" + target}, runOpts{}); err != nil {
				return fmt.Errorf("removing stale ref %s: %w", target, err)
			}
		}
	}

	// Branch doesn't exist (or just deleted). Compute fork
	// point, create the ref, force-checkout.
	baseHash, err := c.computeForkBaseLocked(target, baseBranch, defaultBranch)
	if err != nil {
		return fmt.Errorf("resolving base for new branch %q: %w", target, err)
	}
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + target, baseHash}, runOpts{}); err != nil {
		return fmt.Errorf("creating branch ref %s: %w", target, err)
	}
	return c.forceCheckoutBranchLocked(target)
}

// --- lock-internal helpers (caller already holds c.lock) ---

// forceCheckoutBranchLocked is the shared force-checkout body.
// Caller holds the lock; this just shells out to git checkout -f.
func (c *Clone) forceCheckoutBranchLocked(branch string) error {
	if _, err := runGit(c.workDir, []string{"checkout", "-f", branch}, runOpts{}); err != nil {
		return fmt.Errorf("git: checkout -f %s: %w", branch, err)
	}
	return nil
}

// localRefHashLocked returns the SHA of refs/heads/<branch>, or
// ("", err) when the ref doesn't exist. Distinct from
// LocalBranchHash, which falls back to origin tracking — this
// helper looks ONLY at local refs because callers
// (CheckoutBranchFrom) need to know "is there a local ref to
// reason about" vs "is there a remote-tracking one."
func (c *Clone) localRefHashLocked(branch string) (string, error) {
	out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "refs/heads/" + branch}, runOpts{})
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// resolveOriginRefHashLocked returns the hash of
// refs/remotes/origin/<branch>, or ("", false) when absent.
func (c *Clone) resolveOriginRefHashLocked(branch string) (string, bool) {
	out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", "refs/remotes/origin/" + branch}, runOpts{})
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(out)), true
}

// resolveBaseBranchHashLocked looks up the tip commit of a named
// base branch for the topic-branch fork. Prefers the LOCAL ref
// over origin tracking — recent local commits (template
// auto-commits, unpushed batch submits) live on the local
// branch before origin learns about them, and forking from a
// stale origin would produce a branch whose history misses
// those commits.
func (c *Clone) resolveBaseBranchHashLocked(baseBranch string) (string, bool) {
	if sha, err := c.localRefHashLocked(baseBranch); err == nil && sha != "" {
		return sha, true
	}
	if sha, ok := c.resolveOriginRefHashLocked(baseBranch); ok {
		return sha, true
	}
	return "", false
}

// computeForkBaseLocked decides what commit a fresh branch
// should fork from. Logic mirrors gitv6 verbatim:
//
//   - When baseBranch == "" and origin/<target> exists →
//     materialize from origin (cross-citizen "follow upstream"
//     path).
//   - When baseBranch != "" and resolves → fork from baseBranch.
//   - Otherwise → default-branch ancestry chain (local default,
//     origin default, origin HEAD, repo root).
//
// Returns the chosen SHA or an error when nothing resolves.
func (c *Clone) computeForkBaseLocked(target, baseBranch, defaultBranch string) (string, error) {
	// Cross-citizen "follow upstream" — only when baseBranch is
	// empty so an explicit baseBranch doesn't get overridden by
	// a stale origin ref.
	if baseBranch == "" {
		if sha, ok := c.resolveOriginRefHashLocked(target); ok {
			return sha, nil
		}
	}
	if baseBranch != "" {
		if sha, ok := c.resolveBaseBranchHashLocked(baseBranch); ok {
			return sha, nil
		}
	}
	return c.branchBaseHashLocked(defaultBranch)
}

// branchBaseHashLocked picks the commit a fresh branch should
// fork from when no caller-specified base resolves. Preference:
//  1. refs/heads/<defaultBranch>
//  2. refs/remotes/origin/<defaultBranch>
//  3. refs/remotes/origin/HEAD (symbolic ref)
//  4. The repo's root commit (oldest reachable from HEAD).
//
// Deliberately does NOT fall back to current HEAD — that would
// reintroduce the "silent inheritance" bug gitv6 took pains to
// avoid.
func (c *Clone) branchBaseHashLocked(defaultBranch string) (string, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	for _, ref := range []string{
		"refs/heads/" + defaultBranch,
		"refs/remotes/origin/" + defaultBranch,
		"refs/remotes/origin/HEAD",
	} {
		if out, err := runGit(c.workDir, []string{"rev-parse", "--verify", "-q", ref}, runOpts{}); err == nil {
			return strings.TrimSpace(string(out)), nil
		}
	}
	// Root commit fallback. rev-list --max-parents=0 returns
	// every root commit (one per orphan branch). HEAD restricts
	// to the current history; we use --all only if HEAD itself
	// is unborn.
	out, err := runGit(c.workDir, []string{"rev-list", "--max-parents=0", "HEAD"}, runOpts{})
	if err != nil || strings.TrimSpace(string(out)) == "" {
		return "", fmt.Errorf("local clone at %s has no refs to branch from (remote %q is empty or unseeded)",
			c.workDir, c.remoteURL)
	}
	// Pick the first root (deterministic in practice — there's
	// usually exactly one).
	roots := splitNonEmptyLines(string(out))
	if len(roots) == 0 {
		return "", fmt.Errorf("no root commit reachable from HEAD in %s", c.workDir)
	}
	return roots[0], nil
}

// ensureLocalOrPlantFromOriginLocked resolves a branch name and,
// if no local ref exists but origin tracks it, plants the local
// ref at origin's tip. Returns an error when neither local nor
// origin has the branch.
func (c *Clone) ensureLocalOrPlantFromOriginLocked(branch string) error {
	if _, err := c.localRefHashLocked(branch); err == nil {
		return nil
	}
	originHash, ok := c.resolveOriginRefHashLocked(branch)
	if !ok {
		return fmt.Errorf("ref not found locally or on origin: %s", branch)
	}
	if _, err := runGit(c.workDir, []string{"update-ref", "refs/heads/" + branch, originHash}, runOpts{}); err != nil {
		return fmt.Errorf("plant local ref from origin: %w", err)
	}
	return nil
}
