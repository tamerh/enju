package git

// CheckoutBranchFrom and its supporting helpers — ported verbatim
// from project.Clone.CheckoutBranchFrom during the project-package
// shrinking exercise. The behavior, comments, and edge cases are
// preserved intact; only the receiver type changed (project.Clone
// → git.Clone) and helpers that read project-level fields became
// parameters.

import (
	"fmt"
	"os"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// CheckoutBranchFrom switches the working tree to `branch`,
// creating it locally if it doesn't exist. When `baseBranch` is
// non-empty AND `branch` doesn't yet exist locally or as a
// remote-tracking ref, the new branch is forked from the tip of
// `baseBranch` (resolved via origin/<baseBranch>, then the local
// refs/heads/<baseBranch>). Used by the topic-branch flow:
// per-iteration topic branches fork from the run branch tip,
// not from origin/main, so iter-1's commits land on top of the
// run branch's current state.
//
// When `baseBranch` is empty, behavior degrades to "checkout
// branch": the project's default base (origin/<defaultBranch>,
// then origin/<remote HEAD>, then root) is used as the fork
// point.
//
// `defaultBranch` is the workflow's configured default branch
// name (callers at Workflow level pass w.DefaultBranch()).
//
// Caller MUST hold the project lock (this method runs under
// WithLock).
func (c *Clone) CheckoutBranchFrom(branch, baseBranch, defaultBranch string) error {
	defer c.lock()()

	target := branch
	if target == "" {
		target = defaultBranch
		if target == "" {
			target = "main"
		}
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		return fmt.Errorf("getting worktree: %w", err)
	}
	refName := plumbing.NewBranchReferenceName(target)
	// Already on this branch? No-op.
	if head, err := c.repo.Head(); err == nil && head.Name() == refName {
		return nil
	}
	// Does the branch exist locally (or track a known remote)?
	// Simple checkout, no fork-from dance — UNLESS the existing
	// ref disagrees with the requested baseBranch.
	//
	// Stale-ref guard: a previous attempt at this iteration may
	// have created refs/heads/<branch> at the wrong base (e.g.
	// it tried to fork from baseBranch before that branch was
	// fetchable on this clone, fell through to origin/main, then
	// crashed before committing). Honoring the stale ref blindly
	// is the production bug behind review_domain/iter-2 forking
	// from seed instead of develop_domain/iter-2. When baseBranch
	// is non-empty AND its tip is NOT an ancestor of the existing
	// ref, the ref is stale by definition (a topic branch is
	// supposed to fork from baseBranch); reset it to baseBranch's
	// tip and continue down the create path. The bot's legitimate-
	// retry case still works because that ref's tip would have
	// baseBranch as an ancestor.
	if existing, err := c.repo.Reference(refName, true); err == nil {
		if baseBranch == "" {
			// Upstream-tracking semantics (caller wants the
			// local ref to mirror origin/<target>). If origin
			// has a different hash, the local ref is stale
			// (e.g. a previous broken fix planted it at the
			// wrong commit) and a simple wt.Checkout would
			// land the worktree on the stale tree. Reset by
			// removing the local ref; the fall-through
			// create-new path then re-creates it at origin's
			// tip and Force-checkouts the proper tree.
			if h, ok := c.resolveOriginRefHash(target); ok && h != existing.Hash() {
				if c.logger != nil {
					c.logger.Warn("upstream-tracking ref disagrees with origin; resetting",
						"branch", target,
						"stale_hash", existing.Hash().String(),
						"origin_hash", h.String())
				}
				if err := c.repo.Storer.RemoveReference(refName); err != nil {
					return fmt.Errorf("removing stale upstream-tracking ref %s: %w", target, err)
				}
				// Fall through to create-new path.
			} else {
				return wt.Checkout(&gogit.CheckoutOptions{Branch: refName})
			}
		} else {
			if h, ok := c.resolveBaseBranchHash(baseBranch); ok {
				if c.commitHasAncestor(existing.Hash(), h) {
					return wt.Checkout(&gogit.CheckoutOptions{Branch: refName})
				}
				// Stale: reset and fall through to recreate at baseBranch.
				if c.logger != nil {
					c.logger.Warn("stale topic-branch ref detected; resetting to baseBranch tip",
						"branch", target, "stale_hash", existing.Hash().String(),
						"base_branch", baseBranch, "base_hash", h.String())
				}
				if err := c.repo.Storer.RemoveReference(refName); err != nil {
					return fmt.Errorf("removing stale ref %s: %w", target, err)
				}
			} else {
				// baseBranch given but unresolvable — keep prior
				// behavior (honor existing ref) so we don't make
				// things worse on a transient lookup miss.
				return wt.Checkout(&gogit.CheckoutOptions{Branch: refName})
			}
		}
	}
	// Branch doesn't exist yet. Fork it from the project's
	// BASE — not from workspace HEAD. Forking from HEAD silently
	// inherits whatever branch was checked out last.
	//
	// When the caller passed an explicit baseBranch, prefer that
	// as the fork point so a per-iteration branch lands on top
	// of the run branch's current tip. Falls back to the default
	// base resolution when baseBranch is empty or its tip can't
	// be resolved.
	var baseHash plumbing.Hash
	// Cross-citizen "follow the upstream" path: when the local
	// branch doesn't exist but a remote-tracking ref does, the
	// caller is asking to materialize that branch's content (e.g.
	// reviewer-bot wants to read developer-bot's pushed topic).
	// Create the local at origin/<target>'s tip so the worktree
	// post-checkout reflects the upstream's tree, not seed.
	//
	// Skipped when an explicit baseBranch was passed: that means
	// the caller is forking a NEW branch off baseBranch, so
	// origin/<target> existing would be a stale leftover, not
	// authoritative.
	if baseBranch == "" {
		if h, ok := c.resolveOriginRefHash(target); ok {
			baseHash = h
		}
	}
	if baseHash.IsZero() && baseBranch != "" {
		if h, ok := c.resolveBaseBranchHash(baseBranch); ok {
			baseHash = h
		}
	}
	if baseHash.IsZero() {
		h, err := c.branchBaseHash(defaultBranch)
		if err != nil {
			return fmt.Errorf("resolving base for new branch %q: %w", target, err)
		}
		baseHash = h
	}
	// Create the branch ref at the base hash, then point HEAD
	// at it. go-git's Worktree.Checkout with Create=true uses
	// current HEAD as the starting point; doing the ref dance
	// manually lets us fork from a different commit.
	branchRef := plumbing.NewHashReference(refName, baseHash)
	if err := c.repo.Storer.SetReference(branchRef); err != nil {
		return fmt.Errorf("creating branch ref %s: %w", target, err)
	}
	// Checkout the new branch's tree with Force so files
	// tracked on the PREVIOUS branch but not on the new one
	// get removed from the worktree. To preserve user-authored
	// scratch / gitignored artifacts across the Force, rename
	// non-tracked paths out of workDir before the checkout and
	// rename them back after. See preserve.go for the full
	// rationale.
	preserveDir := c.workDir + PreserveDirSuffix

	// Drain any leftover preserve dir BEFORE renaming fresh
	// content into it. See project.CheckoutBranchFrom history
	// for the in-process / cross-process scenarios this guards.
	if _, statErr := os.Stat(preserveDir); statErr == nil {
		if recErr := RecoverLeftoverPreserve(c.workDir, c.logger); recErr != nil {
			return fmt.Errorf(
				"leftover preserve dir at %s couldn't be drained: %w — review files there and remove (or move aside) before retrying checkout",
				preserveDir, recErr,
			)
		}
		// RecoverLeftoverPreserve cleans up empty dirs but
		// leaves files that collide with current branch
		// content. If anything remains, fail loud rather than
		// risk merging unrelated preservations.
		if _, statErr := os.Stat(preserveDir); statErr == nil {
			return fmt.Errorf(
				"leftover preserve dir at %s holds files that conflict with the current branch — review and remove (or move aside) before retrying checkout",
				preserveDir,
			)
		}
	}

	manifest, err := movePreserveNonTracked(c.repo, c.workDir, preserveDir)
	if err != nil {
		return fmt.Errorf(
			"preserving non-tracked files before checkout failed: %w — partial state at %q; next workspace open will attempt recovery",
			err, preserveDir,
		)
	}
	if err := wt.Checkout(&gogit.CheckoutOptions{
		Branch: refName,
		Force:  true,
	}); err != nil {
		// Checkout failed — restore what we moved so the
		// workspace is usable again.
		_, _ = restoreFromPreserve(c.workDir, preserveDir, manifest)
		return err
	}
	conflicts, restoreErr := restoreFromPreserve(c.workDir, preserveDir, manifest)
	if restoreErr != nil {
		return fmt.Errorf("restoring non-tracked files after checkout: %w", restoreErr)
	}
	if len(conflicts) > 0 && c.logger != nil {
		c.logger.Warn(
			"branch switch preserved non-tracked paths, but some now conflict with tracked paths on the new branch; preserved copies remain in the preserve dir for manual review",
			"preserve_dir", preserveDir,
			"conflict_files", len(conflicts),
		)
	}
	return nil
}

// resolveBaseBranchHash looks up the tip commit of a named base
// branch for the topic-branch flow. Prefers the LOCAL ref
// refs/heads/<baseBranch> over the origin-tracking ref because
// recent local commits (template auto-commits on first
// create_run, batch submits not yet pushed) live on the local
// branch BEFORE origin learns about them — and forking the
// topic from a stale origin/<baseBranch> would produce a
// branch whose history misses those commits, breaking the FF
// invariant when we later merge topic back onto the local run
// branch. Returns (hash, true) on success; (zero, false) when
// neither ref exists, letting the caller fall back to the
// default base resolution.
func (c *Clone) resolveBaseBranchHash(baseBranch string) (plumbing.Hash, bool) {
	if ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(baseBranch), true); err == nil {
		return ref.Hash(), true
	}
	if ref, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", baseBranch), true); err == nil {
		return ref.Hash(), true
	}
	return plumbing.ZeroHash, false
}

// resolveOriginRefHash returns the hash of refs/remotes/origin/<branch>
// when present, or (zero, false) when the remote-tracking ref
// doesn't exist. Used by CheckoutBranchFrom's "follow upstream"
// path so a reviewer's clone that has fetched the developer's
// topic ref can materialize that topic's content into its
// worktree without explicit baseBranch plumbing.
func (c *Clone) resolveOriginRefHash(branch string) (plumbing.Hash, bool) {
	if ref, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", branch), true); err == nil {
		return ref.Hash(), true
	}
	return plumbing.ZeroHash, false
}

// IsAncestor reports whether `ancestor` is reachable from
// `descendant` via parents. Public counterpart to
// commitHasAncestor (which takes plumbing.Hash) — accepts SHA
// strings so callers via the Ops interface don't need to import
// go-git plumbing.
//
// Returns false (no error) when either SHA is invalid / unknown
// to the local object DB; ancestry queries on missing commits
// are not "errors" in the verb sense — the caller's stale-ref
// guard treats them as "not an ancestor" (i.e. the local ref is
// stale enough that we can't reason about it, reseat).
func (c *Clone) IsAncestor(ancestor, descendant string) (bool, error) {
	defer c.lock()()
	a := plumbing.NewHash(ancestor)
	d := plumbing.NewHash(descendant)
	return c.commitHasAncestor(d, a), nil
}

// commitHasAncestor returns true when `ancestor` is reachable
// from `head` by walking parents. Bounded at 200 hops to avoid
// runaway scans on pathological histories — topic branches in
// our model are short (one or two commits over baseBranch), so
// hitting 200 means something is wrong and we should fall
// through to the stale-ref handling.
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

// branchBaseHash picks the commit a fresh branch should fork
// from when the caller asks for a branch that doesn't exist
// yet. Preference order:
//
//  1. refs/heads/<defaultBranch> locally.
//  2. origin/<defaultBranch> for projects with a remote.
//  3. origin/<remote HEAD symbolic ref>.
//  4. The repo's root commit.
//
// Deliberately does NOT fall back to current HEAD (which would
// reintroduce the "silent inheritance" bug).
func (c *Clone) branchBaseHash(defaultBranch string) (plumbing.Hash, error) {
	if defaultBranch == "" {
		defaultBranch = "main"
	}
	if ref, err := c.repo.Reference(plumbing.NewBranchReferenceName(defaultBranch), true); err == nil {
		return ref.Hash(), nil
	}
	if ref, err := c.repo.Reference(plumbing.NewRemoteReferenceName("origin", defaultBranch), true); err == nil {
		return ref.Hash(), nil
	}
	if ref, err := c.repo.Reference(plumbing.NewRemoteHEADReferenceName("origin"), true); err == nil {
		return ref.Hash(), nil
	}
	iter, err := c.repo.Log(&gogit.LogOptions{})
	if err != nil {
		return plumbing.ZeroHash, fmt.Errorf(
			"local clone at %s has no refs to branch from (remote %q is empty or unseeded)",
			c.workDir, c.remoteURL)
	}
	defer iter.Close()
	var root plumbing.Hash
	for {
		cm, err := iter.Next()
		if err != nil {
			break
		}
		root = cm.Hash
	}
	if root.IsZero() {
		return plumbing.ZeroHash, fmt.Errorf(
			"local clone at %s has no commits to branch from (remote %q is empty)",
			c.workDir, c.remoteURL)
	}
	return root, nil
}
