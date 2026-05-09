package gitv6

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing"
)

// CreateBranchAt creates a new local branch ref at the given
// commit SHA. Errors loudly if the branch already exists — caller
// must explicitly DeleteBranch first if they want to replace.
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

// DeleteBranch removes the local branch ref. Idempotent.
func (c *Clone) DeleteBranch(name string) error {
	defer c.lock()()
	refName := plumbing.NewBranchReferenceName(name)
	if err := c.repo.Storer.RemoveReference(refName); err != nil {
		return nil
	}
	return nil
}

// SetBranchTo overwrites a local branch ref to point at sha.
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

// IsAncestor reports whether `ancestor` is reachable from
// `descendant` by walking parents. Used by branch-prep's
// stale-ref validation. Bounded at 200 hops.
func (c *Clone) IsAncestor(ancestor, descendant string) (bool, error) {
	defer c.lock()()
	a := plumbing.NewHash(ancestor)
	d := plumbing.NewHash(descendant)
	return c.commitHasAncestor(d, a), nil
}

// resolveLocalRef resolves a ref name to a hash, looking ONLY in
// refs/heads (not remote-tracking). Used by branch ops that
// mutate the local branch — won't silently advance a local ref
// because origin/* says so.
func (c *Clone) resolveLocalRef(name string) (plumbing.Hash, error) {
	ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(name), true)
	if err != nil {
		return plumbing.ZeroHash, err
	}
	return ref.Hash(), nil
}

// resolveLocalOrPlantFromOrigin resolves a branch name to a hash:
// returns the local SHA if refs/heads/<name> exists, otherwise
// PLANTS refs/heads/<name> from refs/remotes/origin/<name> (when
// the origin tracking ref exists) and returns that SHA.
//
// Returns ErrRefNotFound's underlying error from the repo when
// neither local nor origin tracking has the branch.
func (c *Clone) resolveLocalOrPlantFromOrigin(name string) (plumbing.Hash, error) {
	if hash, err := c.resolveLocalRef(name); err == nil {
		return hash, nil
	}
	remoteRef, rerr := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", name), true)
	if rerr != nil {
		return plumbing.ZeroHash, rerr
	}
	localRef := plumbing.NewBranchReferenceName(name)
	if err := c.repo.Storer.SetReference(plumbing.NewHashReference(localRef, remoteRef.Hash())); err != nil {
		return plumbing.ZeroHash, fmt.Errorf("planting local ref %s from origin: %w", name, err)
	}
	return remoteRef.Hash(), nil
}

// commitHasAncestor returns true when `ancestor` is reachable
// from `head` by walking parents. Bounded at 200 hops to avoid
// runaway scans on pathological histories.
func (c *Clone) commitHasAncestor(head, ancestor plumbing.Hash) bool {
	if head == ancestor {
		return true
	}
	visited := map[plumbing.Hash]bool{}
	frontier := []plumbing.Hash{head}
	for hops := 0; hops < 200 && len(frontier) > 0; hops++ {
		next := []plumbing.Hash{}
		for _, h := range frontier {
			if visited[h] {
				continue
			}
			visited[h] = true
			if h == ancestor {
				return true
			}
			cm, err := c.repo.CommitObject(h)
			if err != nil {
				continue
			}
			next = append(next, cm.ParentHashes...)
		}
		frontier = next
	}
	return false
}

// Checkout switches HEAD to an existing local branch and updates
// the worktree to match. Force is implied: tracked-file
// modifications are overwritten.
//
// v6 difference vs v5: no preserve dance. go-git v6 (PR #1903)
// natively preserves untracked files across Force checkout, so
// the rename-based preserve dir mechanism that v5's Clone needed
// is gone here. Untracked files survive automatically.
//
// Errors:
//   - ErrRefNotFound: branch doesn't exist locally.
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
// or modified.
//
// Errors:
//   - ErrCommitNotFound: sha isn't a real commit.
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
// detachedHash is non-zero. Caller already holds the lock.
//
// Two v6-specific traps avoided:
//
//  1. Worktree.Checkout's setHEADToBranch runs BEFORE Reset, so
//     Reset's prevTree = headTree() reads the NEW branch's tree
//     (HEAD already moved). Tree-diff is empty → tracked-on-old-
//     not-on-new files don't get removed. We bypass Worktree.Checkout
//     entirely and call Reset directly with the right ordering.
//
//  2. wt.Reset(HardReset) moves whatever branch HEAD currently
//     points at, not just HEAD. So if we Reset while still on
//     the OLD branch, the OLD branch's ref gets clobbered to
//     point at the new target — losing any commits that branch
//     had. We avoid this by DETACHING HEAD first (point HEAD
//     directly at the OLD commit hash), then Reset (which now
//     only moves the detached HEAD, no branch refs touched),
//     then re-attach HEAD as a symref to the new branch.
//
// Sequence:
//   1. Resolve target hash.
//   2. Read current HEAD commit hash (oldHash).
//   3. Detach HEAD by setting it to oldHash directly (no symref).
//   4. wt.Reset(HardReset, target) — prevTree = oldHash's tree
//      (correct; HEAD is detached AT oldHash); tree-diff
//      correctly removes prev-only files; untracked preserved.
//   5. Set HEAD = symref to branchRef (or hashRef for detached).
func (c *Clone) checkoutRef(branchRef plumbing.ReferenceName, detachedHash plumbing.Hash) error {
	wt, err := c.repo.Worktree()
	if err != nil {
		return fmt.Errorf("git: worktree handle: %w", err)
	}

	// Step 1: resolve target.
	var targetHash plumbing.Hash
	if branchRef != "" {
		ref, err := c.repo.Reference(branchRef, true)
		if err != nil {
			return fmt.Errorf("%w: %s", ErrRefNotFound, branchRef)
		}
		targetHash = ref.Hash()
	} else {
		targetHash = detachedHash
	}

	// Step 2: read current HEAD commit (best-effort — empty repo
	// or unborn HEAD just skips the detach step).
	var oldHash plumbing.Hash
	if head, err := c.repo.Head(); err == nil {
		oldHash = head.Hash()
	}

	// Step 3: detach HEAD at oldHash so Reset doesn't move any
	// branch ref. Skipped when oldHash is zero (Reset will set
	// HEAD to target either way).
	if !oldHash.IsZero() {
		detached := plumbing.NewHashReference(plumbing.HEAD, oldHash)
		if err := c.repo.Storer.SetReference(detached); err != nil {
			return fmt.Errorf("git: detach HEAD before checkout: %w", err)
		}
	}

	// Step 4: HardReset to target. HEAD detached at oldHash, so
	// prevTree captures correctly; tree-diff removes prev-only
	// tracked files; untracked survive (PR #1903).
	if err := wt.Reset(&gogit.ResetOptions{
		Mode:   gogit.HardReset,
		Commit: targetHash,
	}); err != nil {
		return fmt.Errorf("git: reset to checkout target: %w", err)
	}

	// Step 5: set HEAD to its final form (symref for branch,
	// hashref for detached).
	if branchRef != "" {
		head := plumbing.NewSymbolicReference(plumbing.HEAD, branchRef)
		if err := c.repo.Storer.SetReference(head); err != nil {
			return fmt.Errorf("git: set HEAD to %s: %w", branchRef, err)
		}
	} else {
		head := plumbing.NewHashReference(plumbing.HEAD, detachedHash)
		if err := c.repo.Storer.SetReference(head); err != nil {
			return fmt.Errorf("git: set HEAD detached at %s: %w", detachedHash, err)
		}
	}
	return nil
}

// ResetClean hard-resets the worktree to HEAD and removes any
// untracked files. After this, State() == StateClean. Idempotent.
//
// v6 difference vs v5: no preserve dir suffix to skip during the
// untracked-file walk — preserve.go is gone in this package.
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
			if base == ".git" || base == ".bare.git" || base == ".clone" {
				return filepath.SkipDir
			}
			return nil
		}
		rel, rerr := filepath.Rel(c.workDir, path)
		if rerr != nil {
			return rerr
		}
		if _, ok := tracked[filepath.ToSlash(rel)]; ok {
			return nil
		}
		return os.Remove(path)
	})
}

// SyncIndexToHead updates the index to match HEAD's tree without
// touching the worktree (MixedReset semantics).
func (c *Clone) SyncIndexToHead() error {
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
		Mode:   gogit.MixedReset,
		Commit: head.Hash(),
	}); err != nil {
		return fmt.Errorf("git: mixed reset to HEAD: %w", err)
	}
	return nil
}

// RemoveFiles deletes the given paths from the worktree.
// Idempotent: a path that doesn't exist is silently skipped.
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
