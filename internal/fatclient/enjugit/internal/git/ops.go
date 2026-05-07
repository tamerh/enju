package git

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

	// Worktree — acquire the lock.
	Checkout(branch string) error
	CheckoutCommit(sha string) error
	ResetClean() error
	RemoveFiles(paths []string) error

	// Commit / push / fetch — acquire the lock.
	CommitFiles(req CommitRequest) (CommitResult, error)
	Push(branch string) error
	PushWithVerify(branch, expectedSHA string) error
	Fetch() error

	// Merge — acquire the lock.
	MergeFFOrFail(target, source string) (newTipSHA string, err error)
	MergeWithCommit(target, source, message, authorName, authorEmail string) (newTipSHA string, err error)

	// Diagnostics — read-only.
	CompareToRemote(branches []string) (*RemoteComparison, error)

	// WithLock holds the project lock across the closure. Inside
	// fn, the passed Ops is the same Clone but with re-entrant
	// flag set: nested mutating calls don't try to re-acquire the
	// lock. Used by enjugit for atomic multi-op sequences (e.g.
	// SubmitTaskResult: branch + commit + push as one unit).
	WithLock(fn func(Ops) error) error
}
