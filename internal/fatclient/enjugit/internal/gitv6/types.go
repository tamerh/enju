package gitv6

import (
	"os"
	"time"
)

// CommitInfo describes a single commit for history-walking
// purposes. Returned by LogFile in reverse-chronological order.
type CommitInfo struct {
	Hash    string
	Message string
	Author  string
	Time    time.Time
}


// FileWrite is one file to write to the worktree as part of a
// commit. Mode 0 defaults to 0644.
//
// RepoRelPath is relative to the repo root (e.g. "runs/1/foo/result.md"),
// not absolute and not workdir-relative. Spelled out in the field name
// because callers above this layer can easily get it wrong otherwise.
type FileWrite struct {
	RepoRelPath string
	Content     []byte
	Mode        os.FileMode
}

// TreeEntry is one direct entry of a tree at a given commit.
// Used by ReadTreeEntriesAtCommit + WalkSubtreeBlobsAtCommit
// so callers can classify file vs directory and inspect mode
// without the gogit Tree type leaking out of this package.
type TreeEntry struct {
	// Name is the entry name relative to the directory it was
	// listed from (no path separator).
	Name string

	// IsDir is true when the entry is a subtree (directory).
	IsDir bool

	// Mode is the git tree mode bits, normalized to an os.FileMode.
	// 0o100644 → 0644, 0o100755 → 0755, etc.
	Mode os.FileMode
}

// BlobVisitor is invoked for each regular-file blob found by
// WalkSubtreeBlobsAtCommit. relPath is relative to the walk's
// root subtree (forward-slash separated). Returning an error
// from the visitor stops the walk and surfaces the error.
type BlobVisitor func(relPath string, mode os.FileMode, content []byte) error

// CommitRequest packages inputs for CommitFiles. The message and
// author are passed as raw strings — this layer doesn't compose
// trailers or pick the author. Enjugit does that and hands the
// result here.
type CommitRequest struct {
	// Files written to the worktree before staging. Mode 0 → 0644.
	Files []FileWrite

	// StagePaths is the explicit list of paths to stage. Must be
	// a subset of (or equal to) Files' paths. We never AddGlob(".")
	// because that would sweep unrelated edits into the commit.
	StagePaths []string

	// Message is the complete commit message. Subject on first
	// line, blank line, body. Trailers (if any) are already in
	// the message — this layer doesn't compose them.
	Message string

	// AuthorName / AuthorEmail populate the commit's Author and
	// Committer fields. If both empty, falls back to a generic
	// "Enju git layer <enju-git@localhost>" placeholder so the
	// commit at least has a parseable author. Enjugit always
	// passes real values.
	AuthorName  string
	AuthorEmail string
}

// CommitResult is what CommitFiles returns on success.
type CommitResult struct {
	// SHA of the commit as it landed in the local object DB. May
	// be rewritten later by a rebase before push. PushWithVerify
	// reports the post-push SHA.
	SHA string

	// NoOp is true when none of the requested files would change
	// the worktree (every target path already holds identical
	// content). When NoOp is true, no commit is made and SHA is
	// the SHA of HEAD before the call.
	NoOp bool
}

// RemoteComparison summarizes local-vs-remote sync state for one
// or more branches. Returned by CompareToRemote.
type RemoteComparison struct {
	Branches []BranchStatus
}

// BranchStatus is the local-vs-remote state of one branch.
type BranchStatus struct {
	Name      string
	LocalSHA  string // empty when no local ref
	RemoteSHA string // empty when no remote ref
	State     RemoteState
}

// RemoteState classifies the local-vs-remote relationship for one
// branch. The values match the RemoteComparison output's enum.
type RemoteState int

const (
	// RemoteUnknown is the zero value, used when CompareToRemote
	// hasn't run yet for this branch. Should not appear in
	// returned BranchStatus rows.
	RemoteUnknown RemoteState = iota

	// RemoteInSync — local SHA equals remote SHA.
	RemoteInSync

	// RemoteBehind — remote has commits the local doesn't (local
	// is an ancestor of remote).
	RemoteBehind

	// RemoteAhead — local has commits the remote doesn't (remote
	// is an ancestor of local). Push would fast-forward.
	RemoteAhead

	// RemoteDiverged — neither side is an ancestor of the other.
	// Push would non-FF.
	RemoteDiverged

	// RemoteUnrelated — local and remote share no common ancestor
	// (e.g., bare was rewritten or two unrelated repos pushed to
	// the same name).
	RemoteUnrelated

	// RemoteUnreachable — couldn't reach the remote at all (network,
	// auth). Distinct from any of the above which all imply we
	// successfully read the remote ref.
	RemoteUnreachable
)

func (r RemoteState) String() string {
	switch r {
	case RemoteInSync:
		return "in-sync"
	case RemoteBehind:
		return "behind"
	case RemoteAhead:
		return "ahead"
	case RemoteDiverged:
		return "diverged"
	case RemoteUnrelated:
		return "unrelated"
	case RemoteUnreachable:
		return "unreachable"
	default:
		return "unknown"
	}
}

// WorktreeState classifies the current state of the working tree
// relative to HEAD. Used by verb pre/post contracts.
type WorktreeState int

const (
	// StateClean — worktree matches HEAD exactly. No tracked-file
	// modifications, no untracked files in tracked paths.
	StateClean WorktreeState = iota

	// StateDirtyTracked — at least one tracked file differs from
	// HEAD. May or may not also have untracked files.
	StateDirtyTracked

	// StateDirtyUntracked — tracked files match HEAD but at least
	// one untracked file is present in the worktree.
	StateDirtyUntracked

	// StateDetached — HEAD is detached (no current branch).
	// Returned by CheckoutCommit.
	StateDetached

	// StateMidCheckout — a preserve dir from a crashed prior
	// checkout is still on disk. Most ops refuse in this state
	// until manual recovery clears the preserve dir.
	StateMidCheckout
)

func (s WorktreeState) String() string {
	switch s {
	case StateClean:
		return "clean"
	case StateDirtyTracked:
		return "dirty-tracked"
	case StateDirtyUntracked:
		return "dirty-untracked"
	case StateDetached:
		return "detached"
	case StateMidCheckout:
		return "mid-checkout"
	default:
		return "unknown"
	}
}
