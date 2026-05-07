package git

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CreateBranchAt creates a new local branch ref at the given
// commit SHA. Errors loudly if the branch already exists — caller
// must explicitly DeleteBranch first if they want to replace.
//
// Git operations performed:
//   1. Verify baseSHA is a real commit (lookup in object DB).
//   2. Refuse if refs/heads/<name> already exists.
//   3. Storer.SetReference(refs/heads/<name> → baseSHA).
//
// Worktree state: unchanged (no checkout).
//
// Errors:
//   - ErrBranchExists: refs/heads/<name> already exists.
//   - ErrCommitNotFound: baseSHA isn't a real commit.
func (c *Clone) CreateBranchAt(name, baseSHA string) error {
	defer c.lock()()
	refName := plumbing.NewBranchReferenceName(name)
	if _, err := c.repo.Reference(refName, false); err == nil {
		return fmt.Errorf("%w: %s", ErrBranchExists, name)
	}
	hash := plumbing.NewHash(baseSHA)
	if _, err := c.repo.CommitObject(hash); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, baseSHA)
	}
	ref := plumbing.NewHashReference(refName, hash)
	if err := c.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("git: SetReference %s: %w", name, err)
	}
	return nil
}

// DeleteBranch removes the local branch ref. No-op when the
// branch doesn't exist (idempotent).
//
// Git operations performed:
//   1. Storer.RemoveReference(refs/heads/<name>).
//
// Worktree state: unchanged. Caller is responsible for not
// deleting the branch they're currently on (we don't validate).
func (c *Clone) DeleteBranch(name string) error {
	defer c.lock()()
	refName := plumbing.NewBranchReferenceName(name)
	if err := c.repo.Storer.RemoveReference(refName); err != nil {
		// go-git's "ref not found" is non-fatal for our delete contract.
		return nil
	}
	return nil
}

// SetBranchTo overwrites a local branch ref to point at sha. Used
// by enjugit's stale-ref auto-heal: when a local ref disagrees
// with origin, reset it. Refuses to set to a non-existent commit.
//
// Git operations performed:
//   1. Verify sha is a real commit.
//   2. Storer.SetReference(refs/heads/<name> → sha).
//
// Worktree state: unchanged.
//
// Errors:
//   - ErrCommitNotFound: sha isn't a real commit in local object DB.
func (c *Clone) SetBranchTo(name, sha string) error {
	defer c.lock()()
	hash := plumbing.NewHash(sha)
	if _, err := c.repo.CommitObject(hash); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
	}
	refName := plumbing.NewBranchReferenceName(name)
	ref := plumbing.NewHashReference(refName, hash)
	if err := c.repo.Storer.SetReference(ref); err != nil {
		return fmt.Errorf("git: SetReference %s: %w", name, err)
	}
	return nil
}

// Checkout switches HEAD to an existing local branch and updates
// the worktree to match. Force is implied: tracked-file
// modifications are overwritten. Untracked files are preserved
// via the rename-based preserve dir (multi-GB-safe).
//
// Git operations performed:
//   1. Resolve refs/heads/<branch>; refuse if missing.
//   2. Move untracked files to preserve dir (rename, not copy).
//   3. wt.Checkout({Branch: refs/heads/<branch>, Force: true}).
//   4. Move preserved files back into worktree.
//
// Worktree state: Pre any → Post StateClean (matches branch tip).
//
// Errors:
//   - ErrRefNotFound: branch doesn't exist locally.
//   - ErrPreserveDirCollision: stale preserve dir blocks op.
func (c *Clone) Checkout(branch string) error {
	defer c.lock()()
	refName := plumbing.NewBranchReferenceName(branch)
	if _, err := c.repo.Reference(refName, true); err != nil {
		return fmt.Errorf("%w: %s", ErrRefNotFound, branch)
	}
	return c.checkoutRef(refName, plumbing.ZeroHash)
}

// CheckoutCommit puts HEAD in detached state on the given commit
// and updates the worktree to its tree. NO branch ref is created
// or modified. Used when the caller wants to materialize a
// commit's content without claiming the branch identity (e.g.
// reviewer's pre-handler view of upstream's topic).
//
// Git operations performed:
//   1. Verify sha is a real commit.
//   2. Move untracked files to preserve dir.
//   3. wt.Checkout({Hash: sha, Force: true}).
//   4. Move preserved files back.
//
// Worktree state: Pre any → Post StateDetached.
//
// Errors:
//   - ErrCommitNotFound: sha isn't a real commit.
//   - ErrPreserveDirCollision: stale preserve dir blocks op.
func (c *Clone) CheckoutCommit(sha string) error {
	defer c.lock()()
	hash := plumbing.NewHash(sha)
	if _, err := c.repo.CommitObject(hash); err != nil {
		return fmt.Errorf("%w: %s", ErrCommitNotFound, sha)
	}
	return c.checkoutRef("", hash)
}

// checkoutRef is the shared body of Checkout (branch) and
// CheckoutCommit (detached). Exactly one of branchRef or
// detachedHash is non-zero. Runs the preserve-dance + Force
// checkout. Caller already holds the lock.
func (c *Clone) checkoutRef(branchRef plumbing.ReferenceName, detachedHash plumbing.Hash) error {
	wt, err := c.repo.Worktree()
	if err != nil {
		return fmt.Errorf("git: worktree handle: %w", err)
	}
	preserveDir := c.workDir + preserveDirSuffix

	// Drain leftover preserve dir if it exists.
	if _, statErr := os.Stat(preserveDir); statErr == nil {
		if recErr := recoverLeftoverPreserve(c.workDir, c.logger); recErr != nil {
			return fmt.Errorf("%w: %v", ErrPreserveDirCollision, recErr)
		}
		if _, statErr := os.Stat(preserveDir); statErr == nil {
			return fmt.Errorf("%w: dir at %s", ErrPreserveDirCollision, preserveDir)
		}
	}

	// Move untracked → preserve.
	manifest, err := movePreserveNonTracked(c.repo, c.workDir, preserveDir)
	if err != nil {
		return fmt.Errorf("git: preserve untracked: %w", err)
	}

	// The actual checkout.
	opts := &gogit.CheckoutOptions{Force: true}
	if branchRef != "" {
		opts.Branch = branchRef
	} else {
		opts.Hash = detachedHash
	}
	checkoutErr := wt.Checkout(opts)

	// Restore preserved untracked files (best-effort even if
	// checkout failed — we still want to return the worktree
	// to a usable state).
	conflicts, restoreErr := restoreFromPreserve(c.workDir, preserveDir, manifest)
	if checkoutErr != nil {
		return fmt.Errorf("git: checkout: %w", checkoutErr)
	}
	if restoreErr != nil {
		return fmt.Errorf("git: restore preserved: %w", restoreErr)
	}
	if len(conflicts) > 0 {
		// Non-fatal: log and continue. The conflicting files
		// remain in the preserve dir for manual recovery.
		if c.logger != nil {
			c.logger.Warn("preserved files collide with new branch tree; left in preserve dir",
				"count", len(conflicts), "preserve_dir", preserveDir)
		}
	}
	return nil
}

// ResetClean hard-resets the worktree to HEAD and removes any
// untracked files. After this, State() == StateClean.
//
// Git operations performed:
//   1. wt.Reset(Hard, HEAD).
//   2. Walk the worktree, remove anything not in HEAD's index.
//
// Worktree state: Pre any → Post StateClean.
//
// Used between iterations so the next handler runs on a known
// canvas. Idempotent.
func (c *Clone) ResetClean() error {
	defer c.lock()()
	wt, err := c.repo.Worktree()
	if err != nil {
		return fmt.Errorf("git: worktree handle: %w", err)
	}
	head, err := c.repo.Head()
	if err != nil {
		return fmt.Errorf("%w: HEAD: %v", ErrRefNotFound, err)
	}
	if err := wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.HardReset,
		Commit: head.Hash(),
	}); err != nil {
		return fmt.Errorf("git: hard reset to HEAD: %w", err)
	}
	idx, err := c.repo.Storer.Index()
	if err != nil {
		return fmt.Errorf("git: read index: %w", err)
	}
	tracked := make(map[string]struct{}, len(idx.Entries))
	for _, e := range idx.Entries {
		tracked[e.Name] = struct{}{}
	}
	return filepath.Walk(c.workDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if path == c.workDir {
			return nil
		}
		base := filepath.Base(path)
		if info.IsDir() {
			// Skip git infrastructure dirs and our preserve dir.
			if base == ".git" || base == ".bare.git" || base == ".clone" {
				return filepath.SkipDir
			}
			if len(base) > len(preserveDirSuffix) &&
				base[len(base)-len(preserveDirSuffix):] == preserveDirSuffix {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(c.workDir, path)
		if rerr != nil {
			return rerr
		}
		if _, ok := tracked[filepath.ToSlash(rel)]; ok {
			return nil // tracked file, leave alone (already at HEAD via Reset)
		}
		return os.Remove(path)
	})
}

// RemoveFiles deletes the given paths from the worktree. Used by
// enjugit's WipeIterationWrites to clear a prior iteration's
// declared output files before the next iteration's handler runs.
//
// Idempotent: a path that doesn't exist is silently skipped.
// Paths are relative to workDir.
//
// Git operations performed:
//   1. For each path: os.Remove(workDir/<path>).
//   2. (No git index update — these are worktree-only deletions.)
//
// Worktree state: Pre any → Post unchanged from input minus paths.
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
