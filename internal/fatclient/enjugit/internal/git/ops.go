package git

import "time"

// Ops is the verb surface of the git layer. Workflow code in
// enjugit depends on this interface, not on the concrete *Clone,
// so workflow tests can pass a fake that records calls. Production
// code passes *Clone.
//
// Method docs live on the *Clone implementation. The interface is
// just the contract.
type Ops interface {
	// Reads — never acquire the lock.
	ReadFileAtCommit(sha, path string) (content []byte, found bool, err error)
	ResolveRef(name string) (sha string, err error)
	Head() (sha, branch string, err error)
	LocalBranches() ([]string, error)
	State() WorktreeState

	// Tree reads — read direct entries of one directory or walk a
	// whole subtree's blobs. Both error with ErrCommitNotFound
	// when sha doesn't resolve. Missing dir is `entries=nil, ok=false, err=nil`
	// for ReadTreeEntriesAtCommit; for WalkSubtreeBlobsAtCommit
	// it's also a no-op (visitor not called).
	ReadTreeEntriesAtCommit(sha, dirPath string) (entries []TreeEntry, ok bool, err error)
	WalkSubtreeBlobsAtCommit(sha, dirPath string, visit BlobVisitor) error

	// WalkRecentCommits walks HEAD newest-first, calling visit for
	// up to maxWalk commits. visit returns false to stop early.
	// maxWalk <= 0 walks the whole history. Used by enjugit's
	// batch submit to map task_id → SHA via Enju-Task-Complete
	// trailers (rebase-stable key).
	WalkRecentCommits(maxWalk int, visit func(sha, message string) bool) error

	// LogFile returns commits that touched relPath, newest-first.
	// Used by per-file history readers (enju_get_artifact_history).
	LogFile(relPath string) ([]CommitInfo, error)

	// Refs / branches — acquire the lock.
	CreateBranchAt(name, baseSHA string) error
	DeleteBranch(name string) error
	SetBranchTo(name, sha string) error

	// IsAncestor reports whether `ancestor` is reachable from
	// `descendant` by walking parents. Used by branch-prep's
	// stale-ref validation: when a local topic ref exists and the
	// caller supplied a preferred fork base, we check whether the
	// base's tip is in the topic's ancestry; if not, the ref is
	// stale and gets reseated. Both arguments are SHA strings.
	IsAncestor(ancestor, descendant string) (bool, error)

	// Worktree — acquire the lock.
	Checkout(branch string) error
	CheckoutCommit(sha string) error
	ResetClean() error
	SyncIndexToHead() error
	RemoveFiles(paths []string) error

	// CheckoutBranchFrom is the meaty branch-checkout: stale-ref
	// guard, fork-from-baseBranch, Force checkout with non-tracked
	// preservation. See checkout_branch_from.go.
	CheckoutBranchFrom(branch, baseBranch, defaultBranch string) error

	// Commit / push / fetch — acquire the lock.
	CommitFiles(req CommitRequest) (CommitResult, error)

	// PlumbingCommit builds a commit object directly via the
	// object store WITHOUT touching HEAD, .git/index, or the
	// working tree. Returns the commit SHA. Caller is
	// responsible for advancing a ref via UpdateRef. Designed
	// for parallel compute: N goroutines on the same Clone can
	// each invoke this concurrently because no shared mutable
	// state is touched. See plumbing_commit.go.
	PlumbingCommit(req PlumbingCommitRequest) (string, error)

	// UpdateRef atomically sets refs/heads/<name> to newSHA.
	// expectedOldSHA="" allows any current value (including
	// non-existent ref); non-empty triggers compare-and-swap
	// and fails when the ref's current value doesn't match.
	UpdateRef(name, newSHA, expectedOldSHA string) error
	Push(branch string) error
	PushAllRefs(force bool) error
	PushWithVerify(branch, expectedSHA string) error
	RebaseOnRemote(branch string) error
	Fetch() error

	// EnsureOrigin self-heals the on-disk .git/config when the
	// origin remote section is missing or mismatched. Band-aid
	// for the dual-handle bug (#381); see Clone.EnsureOrigin
	// docstring for context.
	EnsureOrigin(url string) error
	// RemoveOrigin deletes the origin remote when present;
	// idempotent no-op when absent.
	RemoveOrigin() error

	// Per-branch fetch + pull (reconcile / claim path). FetchBranch
	// updates only refs/remotes/origin/<branch>; PullBranch fetches
	// and merges into the local branch. Both no-op when the named
	// branch doesn't exist on origin.
	FetchBranch(branch string) error
	PullBranch(branch string) error

	// LocalBranchHash returns the SHA of refs/heads/<branch>,
	// falling back to refs/remotes/origin/<branch>. Empty string
	// when neither resolves.
	LocalBranchHash(branch string) (string, error)

	// ScanBranchSince walks commits on origin/<branch> (or
	// refs/heads/<branch>) newer than `since`, calling visit for
	// each in chronological order. Returns the new tip SHA.
	// See git.Clone.ScanBranchSince for the cursor semantics.
	ScanBranchSince(branch, since string, visit func(sha, message string)) (newTip string, err error)

	// ReadFile reads a worktree file at a repo-relative path.
	ReadFile(repoRelPath string) ([]byte, error)

	// CheckoutBranch is a no-op when branch is "" (matches
	// project.PullBranchWithReconcile's "skip switch when empty"
	// semantics), else equivalent to Checkout.
	CheckoutBranch(branch string) error

	// Merge — acquire the lock.
	MergeFFOrFail(target, source string) (newTipSHA string, err error)
	MergeWithCommit(target, source, message, authorName, authorEmail string) (newTipSHA string, err error)

	// Diagnostics — read-only.
	CompareToRemote(branches []string) (*RemoteComparison, error)

	// RemoteBranchHash queries origin via ls-remote and returns
	// the remote tip SHA for the named branch, or "" when the
	// branch is absent on the remote. Errors when the remote is
	// unreachable.
	RemoteBranchHash(branch string) (string, error)

	// CompareCommits classifies the relationship between two
	// commits in the local object DB. Both SHAs must be non-empty;
	// callers handle missing-side cases (empty local / empty
	// remote) before calling. When remoteSHA is unknown to the
	// local object DB, CompareCommits performs a single Fetch
	// against origin and retries; if still unresolvable, it
	// returns RemoteDiverged with zero counts (best effort, since
	// we can't compute a merge base). Used by sync-preflight UX.
	CompareCommits(localSHA, remoteSHA string) (CommitCompareResult, error)

	// Clone metadata — read-only state queries with no git
	// operations behind them. Live on Ops (not via type assertion
	// to *Clone) so fakes can return canned values and Workflow
	// can program against the interface uniformly.
	WorkDir() string
	RemoteURL() string
	LastPushAt() time.Time
	LastPushError() string
	HeadCommitTime() time.Time

	// WithLock holds the project lock across the closure. Inside
	// fn, the passed Ops is the same Clone but with re-entrant
	// flag set: nested mutating calls don't try to re-acquire the
	// lock. Used by enjugit for atomic multi-op sequences (e.g.
	// SubmitTaskResult: branch + commit + push as one unit).
	WithLock(fn func(Ops) error) error
}
