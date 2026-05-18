package enjugit

// branch_cleanup.go — on-demand cleanup of merged run and iter
// branches.
//
// After a run completes and its run branch is merged into base,
// the run branch plus all of its per-task iteration topic
// branches are redundant: their commits are permanently
// reachable through base's ancestry, so the branch refs are
// just noise in `git branch`, tab-completion, and the
// worktree-parked-on-run-branch footgun.
//
// Selection uses two independent gates per branch:
//
//  1. Candidate set: run.Branch (exact) plus all local branches
//     whose name starts with "<seq>-<slug>/". The prefix is a
//     heuristic — it can include branches from a sibling run that
//     shares the same seq+slug after a coord wipe. This is
//     intentional: gate 2 is the actual safety guarantee.
//
//  2. Merged gate: the branch tip must be an ancestor of the
//     base branch's current tip (IsAncestor check). Only
//     base-reachable refs are ever removed — their commits are
//     permanently reachable through base regardless of whether
//     the ref exists. The identity heuristic scopes the sweep
//     to one run's namespace; the ancestor gate is what makes
//     removal safe.
//
// Unmerged iter branches (request_changes loops, rejected
// iterations, aborted runs) fail the ancestor check and are
// ALWAYS preserved. Their commits are not in base; the branch
// ref is the only thing keeping them reachable — the
// "what we tried and rejected" audit trail.
//
// Two cleanup modes:
//
//   - archive: move refs/heads/<name> to refs/enju/archive/runs/<name>.
//     Gone from `git branch` and tab-completion; fully reversible
//     with `git update-ref`. Zero data risk.
//
//   - prune: delete refs/heads/<name>. Commits stay reachable
//     via base's ancestry; the ref itself is gone. Irreversible
//     but safe for merged branches (git reflog still has the SHA
//     for a while). Use when you want full hygiene.

import (
	"fmt"
	"strings"

	"github.com/enju-ai/enju/internal/common/wire"
)

// CleanupMode controls what happens to merged run branches.
type CleanupMode string

const (
	// CleanupModeNone leaves all branches untouched (default today).
	CleanupModeNone CleanupMode = "none"
	// CleanupModeArchive moves merged refs to refs/enju/archive/runs/<name>.
	// Fully reversible. Preferred conservative option.
	CleanupModeArchive CleanupMode = "archive"
	// CleanupModePrune deletes merged refs entirely.
	// Commits remain reachable via base; the ref is gone.
	CleanupModePrune CleanupMode = "prune"
)

// ParseCleanupMode parses a string into a CleanupMode. Returns
// (mode, true) on match, ("", false) for unrecognised values.
func ParseCleanupMode(s string) (CleanupMode, bool) {
	switch CleanupMode(strings.ToLower(s)) {
	case CleanupModeNone:
		return CleanupModeNone, true
	case CleanupModeArchive:
		return CleanupModeArchive, true
	case CleanupModePrune:
		return CleanupModePrune, true
	default:
		return "", false
	}
}

// BranchCleanupResult tallies what happened during a cleanup pass.
type BranchCleanupResult struct {
	Mode     CleanupMode
	Base     string   // base branch used for the ancestor check
	Archived []string // branches moved to refs/enju/archive/runs/
	Pruned   []string // branches deleted
	Skipped  []string // not yet merged into base — preserved
	Errors   []string // per-branch non-fatal errors
}

// CleanupRunBranches archives or prunes merged run and iter
// branches for the given terminal runs.
//
// baseBranch is the project's default branch — the merge target
// used in the ancestor check. The caller must pass only terminal
// runs (completed / failed / terminated); this function silently
// skips any run whose state is not terminal as an extra safety
// layer.
//
// The function is non-fatal per branch: individual errors are
// collected in BranchCleanupResult.Errors and the sweep continues.
// A returned error means a prerequisite failed (e.g. baseBranch
// not found locally).
func (w *Workflow) CleanupRunBranches(runs []wire.Run, mode CleanupMode) (*BranchCleanupResult, error) {
	res := &BranchCleanupResult{Mode: mode, Base: w.DefaultBranch()}

	if mode == CleanupModeNone || len(runs) == 0 {
		return res, nil
	}

	baseBranch := w.DefaultBranch()
	if baseBranch == "" {
		return nil, fmt.Errorf("cleanup: no default branch configured for this project")
	}

	// Resolve base branch tip once — all ancestor checks use this SHA.
	baseSHA, err := w.git.LocalBranchHash(baseBranch)
	if err != nil || baseSHA == "" {
		return nil, fmt.Errorf("cleanup: cannot resolve base branch %q: %v", baseBranch, err)
	}

	// List all local branches once — avoids N git calls in the loop.
	allLocal, err := w.git.LocalBranches()
	if err != nil {
		return nil, fmt.Errorf("cleanup: list local branches: %w", err)
	}

	// Build a set for O(1) membership tests and a slice for prefix scans.
	localSet := make(map[string]bool, len(allLocal))
	for _, b := range allLocal {
		localSet[b] = true
	}

	for _, run := range runs {
		if !wire.IsTerminalRunState(run.State) {
			continue
		}

		// Collect candidate branches for this run:
		//   1. The run branch itself (exact match from coord record).
		//   2. All iter/topic branches whose name starts with "<seq>-<slug>/".
		var candidates []string

		if run.Branch != "" && localSet[run.Branch] {
			candidates = append(candidates, run.Branch)
		}

		if run.Slug != "" {
			prefix := fmt.Sprintf("%d-%s/", run.Seq, run.Slug)
			for _, b := range allLocal {
				if strings.HasPrefix(b, prefix) {
					candidates = append(candidates, b)
				}
			}
		}

		for _, branch := range candidates {
			if err := w.cleanupBranch(branch, baseSHA, mode, res); err != nil {
				res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", branch, err))
			}
		}
	}

	return res, nil
}

// cleanupBranch handles a single candidate branch: checks ancestry,
// then archives or prunes if merged. Appends to res in-place.
func (w *Workflow) cleanupBranch(branch, baseSHA string, mode CleanupMode, res *BranchCleanupResult) error {
	branchSHA, err := w.git.LocalBranchHash(branch)
	if err != nil || branchSHA == "" {
		// Branch disappeared between the list and now — skip quietly.
		return nil
	}

	// Gate: only act on branches whose tip is an ancestor of base.
	// This is the correctness invariant: if the branch tip is in
	// base's ancestry, every commit on it is permanently reachable
	// through base. Unmerged branches (rejected iters, aborted
	// runs) fail this check and are left untouched.
	merged, err := w.git.IsAncestor(branchSHA, baseSHA)
	if err != nil {
		return fmt.Errorf("ancestor check: %w", err)
	}
	if !merged {
		res.Skipped = append(res.Skipped, branch)
		return nil
	}

	switch mode {
	case CleanupModeArchive:
		archiveRef := "refs/enju/archive/runs/" + branch
		if err := w.git.CreateRef(archiveRef, branchSHA); err != nil {
			return fmt.Errorf("create archive ref: %w", err)
		}
		// CAS delete: if the branch tip advanced between LocalBranchHash
		// and here, the delete fails cleanly — the archive ref holds the
		// old vetted SHA, but the newer commit would have no other ref and
		// isn't base-reachable, so an unconditional delete would lose it.
		if err := w.git.DeleteBranchCAS(branch, branchSHA); err != nil {
			return fmt.Errorf("delete head after archive: %w", err)
		}
		res.Archived = append(res.Archived, branch)

	case CleanupModePrune:
		// CAS delete: only succeeds if the branch tip is still the
		// SHA we vetted with IsAncestor. A concurrent claim that
		// advanced the branch past the checked SHA causes a clean
		// error rather than silently destroying newer commits.
		if err := w.git.DeleteBranchCAS(branch, branchSHA); err != nil {
			return fmt.Errorf("delete branch: %w", err)
		}
		res.Pruned = append(res.Pruned, branch)
	}

	return nil
}
