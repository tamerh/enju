package git

// scan.go — fetch-path primitives for the reconcile flow:
// FetchBranch (refresh origin tracking ref), ScanBranchSince
// (walk commits since cursor, return per-commit messages),
// PullBranch (fetch + merge into local branch), and the small
// helpers reconcile needs (RemoteBranchHash, LocalBranchHash,
// ReadFile, GitOriginURL).
//
// Trailer parsing is intentionally kept out of this layer:
// ScanBranchSince returns *raw commit messages* via a callback so
// callers can plug their own ParseEnjuTrailers. The higher-level
// Workflow.ScanBranchSince composes the two.

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
	"github.com/go-git/go-git/v5/plumbing/storer"
)

// FetchBranch updates `refs/remotes/origin/<branch>` from the
// remote without touching the worktree or local branch refs.
// Returns nil on success, including when the branch doesn't
// exist remotely (a brand-new branch yet to be pushed) — the
// caller's scanner falls back to local refs in that case.
//
// Distinct from Fetch (which does a full refspec fetch): this
// only refreshes ONE branch's tracking ref. Used by the scan
// path so it picks up the latest tip without a wide fetch.
func (c *Clone) FetchBranch(branch string) error {
	defer c.lock()()
	if branch == "" {
		return fmt.Errorf("git: FetchBranch: branch required (caller should resolve to default)")
	}
	remoteSHA, err := c.remoteBranchHashLocked(branch)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	refName := plumbing.NewBranchReferenceName(branch)
	remoteRefName := plumbing.NewRemoteReferenceName("origin", branch)
	refSpec := config.RefSpec(fmt.Sprintf("+%s:%s", refName, remoteRefName))
	err = c.repo.Fetch(&gogit.FetchOptions{
		RemoteName: "origin",
		RefSpecs:   []config.RefSpec{refSpec},
		Auth:       SSHAuthMethod(c.remoteURL),
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("git: fetch branch %s: %w", branch, err)
	}
	return nil
}

// PullBranch fetches the named branch from origin and fast-
// forwards the local branch ref to match. Distinct from
// FetchBranch (which only updates origin tracking) — Pull
// updates the LOCAL branch and the worktree.
//
// No-op when remote doesn't yet have this branch (brand-new
// branch yet to be pushed).
func (c *Clone) PullBranch(branch string) error {
	defer c.lock()()
	if branch == "" {
		// Empty branch is a caller bug — Workflow.PullBranch
		// resolves "" to the default before calling. If we got
		// here with empty, surface it instead of silently
		// no-oping.
		return fmt.Errorf("git: PullBranch: branch required (caller should resolve to default)")
	}
	remoteSHA, err := c.remoteBranchHashLocked(branch)
	if err != nil {
		return err
	}
	if remoteSHA == "" {
		return nil
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return fmt.Errorf("git: PullBranch: worktree: %w", err)
	}
	refName := plumbing.NewBranchReferenceName(branch)
	err = wt.Pull(&gogit.PullOptions{
		RemoteName:    "origin",
		ReferenceName: refName,
		SingleBranch:  true,
		Auth:          SSHAuthMethod(c.remoteURL),
	})
	if err != nil && err != gogit.NoErrAlreadyUpToDate {
		return fmt.Errorf("git: pull branch %s: %w", branch, err)
	}
	return nil
}

// RemoteBranchHash queries origin via ls-remote and returns the
// SHA of the named branch, or empty string when the remote
// doesn't carry it.
func (c *Clone) RemoteBranchHash(branch string) (string, error) {
	defer c.lock()()
	return c.remoteBranchHashLocked(branch)
}

// remoteBranchHashLocked is the lock-free variant for callers
// that already hold c.lock (FetchBranch / PullBranch).
func (c *Clone) remoteBranchHashLocked(branch string) (string, error) {
	rem, err := c.repo.Remote("origin")
	if err != nil {
		return "", fmt.Errorf("git: remote-branch-hash: %w", err)
	}
	refs, err := rem.List(&gogit.ListOptions{
		Auth: SSHAuthMethod(c.remoteURL),
	})
	if err != nil {
		return "", fmt.Errorf("git: ls-remote: %w", err)
	}
	target := plumbing.NewBranchReferenceName(branch)
	for _, r := range refs {
		if r.Name() == target {
			return r.Hash().String(), nil
		}
	}
	return "", nil
}

// LocalBranchHash returns the SHA of the named local branch
// ref, falling back to refs/remotes/origin/<branch> when the
// local ref doesn't exist. Empty string when neither exists.
func (c *Clone) LocalBranchHash(branch string) (string, error) {
	if branch == "" {
		return "", fmt.Errorf("git: LocalBranchHash: branch required")
	}
	localRef := plumbing.NewBranchReferenceName(branch)
	if ref, err := c.repo.Reference(localRef, true); err == nil {
		return ref.Hash().String(), nil
	}
	remoteRef := plumbing.NewRemoteReferenceName("origin", branch)
	if ref, err := c.repo.Reference(remoteRef, true); err == nil {
		return ref.Hash().String(), nil
	}
	return "", nil
}

// ScanBranchSince walks commits on `refs/remotes/origin/<branch>`
// (falling back to refs/heads/<branch> when origin tracking is
// absent) newer than `since` (exclusive) back to tip, calling
// visit(sha, message) for each. Returns the new tip SHA — the
// caller persists this as the next cursor.
//
// Semantics match project.ScanBranchSince:
//   - since == "" → first scan: return tip + don't visit
//   - since == tip → no-op: return tip + don't visit
//   - since unreachable → walk from tip without stop (force-push,
//     rebase): caller's reconcile is idempotent, duplicates OK
//   - otherwise → walk since..tip exclusive of since
//
// Visits in chronological order (ancestor → tip).
func (c *Clone) ScanBranchSince(branch, since string, visit func(sha, message string)) (newTip string, err error) {
	defer c.lock()()
	if branch == "" {
		return since, fmt.Errorf("git: ScanBranchSince: branch required")
	}
	remoteRef := plumbing.NewRemoteReferenceName("origin", branch)
	ref, err := c.repo.Reference(remoteRef, true)
	if err != nil {
		// Fall back to local ref (no origin or fresh branch).
		localRef := plumbing.NewBranchReferenceName(branch)
		ref, err = c.repo.Reference(localRef, true)
		if err != nil {
			return since, nil // genuinely unknown; not an error
		}
	}
	tip := ref.Hash().String()
	if since == "" || since == tip {
		return tip, nil
	}

	// stopOnSince: if `since` resolves to a commit we have, stop
	// the walk when we reach it. Otherwise walk to the root
	// (force-push/rebase scenario).
	_, sinceErr := c.repo.CommitObject(plumbing.NewHash(since))
	stopOnSince := sinceErr == nil

	iter, err := c.repo.Log(&gogit.LogOptions{From: ref.Hash()})
	if err != nil {
		return since, fmt.Errorf("git: log %s: %w", branch, err)
	}
	defer iter.Close()

	// Collect newest-first, then reverse to chronological.
	type commit struct{ sha, msg string }
	var collected []commit
	walkErr := iter.ForEach(func(co *object.Commit) error {
		if stopOnSince && co.Hash.String() == since {
			return storer.ErrStop
		}
		collected = append(collected, commit{sha: co.Hash.String(), msg: co.Message})
		return nil
	})
	if walkErr != nil && !errors.Is(walkErr, storer.ErrStop) {
		return since, fmt.Errorf("git: walk log: %w", walkErr)
	}
	for i, j := 0, len(collected)-1; i < j; i, j = i+1, j-1 {
		collected[i], collected[j] = collected[j], collected[i]
	}
	for _, co := range collected {
		visit(co.sha, co.msg)
	}
	return tip, nil
}

// ReadFile reads a file from the working tree at a
// repo-relative path. Mirrors project.Clone.ReadFile.
func (c *Clone) ReadFile(repoRelPath string) ([]byte, error) {
	full := filepath.Join(c.workDir, repoRelPath)
	return os.ReadFile(full)
}

// CheckoutBranch is the reconcile-path entry point: switches
// the worktree to the named branch. When branch == "" it's a
// no-op (matches project.PullBranchWithReconcile semantics so
// callers don't special-case).
//
// Distinct from Checkout: this is a SOFT checkout (no Force,
// no preserve dance). go-git refuses if the worktree has
// uncommitted changes — which is the right behavior for
// reconcile, where dirty trees indicate concurrent work the
// caller should handle, not a stale branch to wipe. State-prep
// flows (iter-N branch creation, post-iteration cleanup) still
// use the force-flavored Checkout directly.
//
// TODO(post-project-retirement): rename for clarity. Current
// `Checkout` (force) vs `CheckoutBranch` (soft) doesn't
// telegraph the difference — a reader can't tell which is
// destructive from the name. Plan: rename Checkout →
// ForceCheckout, and rename this to Checkout (the safe default).
// Deferred to avoid churning ~10 callsites in state_prep.go /
// producing.go mid-migration.
//
// Why soft: tests + ad-hoc operator workflow chmod files in the
// worktree (e.g. setting +x on committed scripts that git
// stores at 0644). A force checkout resets those mode bits;
// the next exec gets "permission denied". Project's
// PullBranchWithReconcile.CheckoutBranch was non-force and
// silently no-op'd on dirty trees — preserving the chmod as a
// side effect. We mirror that here.
func (c *Clone) CheckoutBranch(branch string) error {
	defer c.lock()()
	if branch == "" {
		return nil
	}
	// Auto-track-from-origin: a fresh per-bot clone has
	// refs/remotes/origin/<branch> after fetch but no local
	// refs/heads/<branch> until something explicitly creates one.
	// Plant the local ref from origin tracking before checking
	// out, so first-claim flows on a brand-new bot clone don't
	// fail with "ref not found" against a branch that origin
	// clearly has. Same pattern as MergeFFOrFail's target
	// resolution. Returns ErrRefNotFound only when neither
	// local nor origin tracking has the branch — the genuinely-
	// unknown case.
	if _, err := c.resolveLocalOrPlantFromOrigin(branch); err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, branch)
	}
	refName := plumbing.NewBranchReferenceName(branch)
	// Same-branch fast path: HEAD already on the target. wt.Checkout
	// would refuse on a dirty tree, but a same-branch transition is
	// a no-op in real terms — nothing to update. Skip the call so
	// dirty trees (chmod-edited scripts, in-progress edits) don't
	// fail pre-claim reconcile.
	if head, err := c.repo.Head(); err == nil && head.Name() == refName {
		return nil
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return fmt.Errorf("git: worktree handle: %w", err)
	}
	return wt.Checkout(&gogit.CheckoutOptions{Branch: refName})
}
