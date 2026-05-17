package enjugit

// scan.go — Workflow-side passthroughs for the reconcile/claim
// path's read primitives. Service composes them with cursor
// state + coord POSTs in PullBranchWithReconcileWF.
//
// All methods are thin wrappers over the git layer with typed
// error translation. Comments live on git.Clone — see scan.go
// in internal/git/ for semantics.

// CommitTrailer pairs a commit SHA with its parsed Enju trailers.
// Used by Workflow.ScanBranchSince so service-side reconcile can
// build the /tasks/reconcile body without touching git internals.
type CommitTrailer struct {
	CommitSHA string
	Trailers  EnjuTrailers
}

// ScanResult bundles ScanBranchSince's outputs. NewTip is the
// branch's tip SHA after the scan (the value the caller persists
// as the next cursor). Trailers are the per-commit Enju trailers
// for commits since the input cursor, in chronological order.
//
// Same shape as SubmitResult / batch results — keeps the verb
// surface uniform: every multi-output verb returns (*XxxResult, error).
type ScanResult struct {
	NewTip   string
	Trailers []CommitTrailer
}

// FetchBranch refreshes refs/remotes/origin/<branch> from the
// remote without touching the worktree. No-op when the named
// branch doesn't exist on origin yet. Empty branch resolves to
// the workflow's default branch so callers without a specific
// branch context fall back to the project default rather than
// silently skipping the fetch. No-op when origin is unset — the
// post-Phase-8 single-store layout has no remote to fetch from.
func (w *Workflow) FetchBranch(branch string) error {
	if w.git.RemoteURL() == "" {
		return nil
	}
	resolved := w.resolveBranch(branch)
	return w.wrapGitError("fetch branch", resolved, w.git.FetchBranch(resolved))
}

// PullBranch fetches + fast-forwards the local branch. No-op
// when the named branch doesn't exist on origin yet. Empty
// branch → default branch, same rationale as FetchBranch.
// No-op when origin is unset — the local branch tip IS the
// source of truth in the post-Phase-8 single-store layout.
func (w *Workflow) PullBranch(branch string) error {
	if w.git.RemoteURL() == "" {
		return nil
	}
	resolved := w.resolveBranch(branch)
	return w.wrapGitError("pull branch", resolved, w.git.PullBranch(resolved))
}

// resolveBranch returns branch when non-empty, else the
// workflow's default branch (defaultBranch field, falling
// back to convs.DefaultRunBranch). Mirrors
// project.Clone.resolveBranch so callers that pass "" get the
// same "use project default" semantics they got pre-port.
func (w *Workflow) resolveBranch(branch string) string {
	if branch != "" {
		return branch
	}
	return w.DefaultBranch()
}

// LocalBranchHash returns the SHA of refs/heads/<branch>,
// falling back to refs/remotes/origin/<branch>. Empty string
// when neither resolves.
func (w *Workflow) LocalBranchHash(branch string) (string, error) {
	sha, err := w.git.LocalBranchHash(branch)
	return sha, translateGitError("local branch hash", err)
}

// IsAncestor reports whether `ancestor` is an ancestor of
// `descendant` (git merge-base --is-ancestor). Empty or unknown
// SHAs resolve to (false, nil), matching LocalBranchHash's
// tolerant contract — callers branch on the bool, not the error.
func (w *Workflow) IsAncestor(ancestor, descendant string) (bool, error) {
	ok, err := w.git.IsAncestor(ancestor, descendant)
	return ok, translateGitError("is ancestor", err)
}

// CheckoutBranch is a no-op when branch is "" (matches the
// reconcile path's "skip switch when empty" semantics), else
// equivalent to Checkout.
func (w *Workflow) CheckoutBranch(branch string) error {
	return w.wrapGitError("checkout branch", branch, w.git.CheckoutBranch(branch))
}

// CheckoutBranchFrom switches the worktree to `branch`,
// creating it locally if absent. When `baseBranch` is non-empty
// AND the branch is being created fresh, the new branch forks
// from baseBranch's tip (resolved via local refs/heads/<base>,
// then refs/remotes/origin/<base>). Empty `branch` falls back
// to the workflow's default branch. Stale-ref guard reseats
// existing topic branches that don't have baseBranch in their
// ancestry — a previous bad fork would otherwise replay across
// retries. Force-checkout under non-tracked file preservation
// (renames non-tracked paths into a sibling preserve dir before
// the swap, restores them after) so multi-GB gitignored
// artifacts survive the branch switch unchanged.
//
// Atomic: the entire ref/worktree dance runs under one
// WithLock call inside git.Clone.CheckoutBranchFrom.
func (w *Workflow) CheckoutBranchFrom(branch, baseBranch string) error {
	return w.wrapGitError("checkout branch from", branch,
		w.git.CheckoutBranchFrom(branch, baseBranch, w.DefaultBranch()))
}

// ReadFile reads a file from the worktree at a repo-relative
// path. Used by claim.go's previous-submission read fallback.
func (w *Workflow) ReadFile(repoRelPath string) ([]byte, error) {
	body, err := w.git.ReadFile(repoRelPath)
	return body, translateGitError("read file", err)
}

// ScanBranchSince walks origin/<branch> (or refs/heads/<branch>)
// commits newer than `since`, parses the Enju trailers from each
// commit message, and returns a ScanResult with the new tip SHA
// + per-commit trailers in chronological order. Cursor semantics:
// see git.Clone.ScanBranchSince.
//
// Workflow-level wrapper combines the raw git walk with
// ParseEnjuTrailers so service callers don't have to thread the
// commit message themselves.
func (w *Workflow) ScanBranchSince(branch, since string) (*ScanResult, error) {
	var found []CommitTrailer
	tip, gerr := w.git.ScanBranchSince(branch, since, func(sha, message string) {
		t := ParseEnjuTrailers(message)
		if t.TaskID != "" {
			found = append(found, CommitTrailer{
				CommitSHA: sha,
				Trailers:  t,
			})
		}
	})
	if gerr != nil {
		return nil, translateGitError("scan branch since", gerr)
	}
	return &ScanResult{NewTip: tip, Trailers: found}, nil
}
