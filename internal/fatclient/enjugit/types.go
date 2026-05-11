package enjugit

import (
	"time"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// CommitInfo describes a single commit for history-walking
// purposes. Returned by Workflow.LogFile in reverse-chronological
// order (newest first). Native to enjugit so service-layer
// callers don't transitively depend on internal/git's struct
// shape — keeps the layer boundary load-bearing.
type CommitInfo struct {
	Hash    string
	Message string
	Author  string
	Time    time.Time
}

// ForkPoint names where a new iteration branch should fork from.
// Replaces the previous "magic baseBranch parameter" overload
// that caused the cross-citizen-fork bug class. With a typed
// enum, callers spell out the intent and wrong choices become
// obvious code-review catches.
type ForkPoint int

const (
	// ForkUnknown is the zero value; should not appear in correct
	// code. Verbs reject it explicitly.
	ForkUnknown ForkPoint = iota

	// ForkFromRunBranch — the new iteration branch forks from the
	// run branch's tip. Used for develop iterations after invalidate
	// (fresh start).
	ForkFromRunBranch

	// ForkFromUpstreamTopic — the new iteration branch forks from
	// the upstream task's topic-branch tip. Used for review
	// iterations so the reviewer's own commits sit on top of the
	// developer's content.
	ForkFromUpstreamTopic

	// ForkFromPriorIteration — the new iteration branch forks from
	// the immediately-prior iteration's tip. Reserved for
	// stack-on-top revision semantics; not yet wired.
	ForkFromPriorIteration
)

func (f ForkPoint) String() string {
	switch f {
	case ForkFromRunBranch:
		return "from-run-branch"
	case ForkFromUpstreamTopic:
		return "from-upstream-topic"
	case ForkFromPriorIteration:
		return "from-prior-iteration"
	default:
		return "unknown"
	}
}

// SubmitRequest packages a single task submission. Service builds
// this from the claim metadata + the handler's output and hands
// it to Workflow.SubmitTaskResult.
type SubmitRequest struct {
	// Identification
	TaskID  string
	IterSeq int

	// Branch placement (composed by Conventions.BranchName when
	// BranchOverride is empty)
	RunSeq      int
	RunSlug     string
	TaskDef     string
	InstanceKey string
	RunBranch   string // run branch name (where merge eventually lands)

	// BranchOverride bypasses Conventions.BranchName when set.
	// Used by submit flows that don't follow the topic-branch
	// pattern: vote/review actions land directly on the run
	// branch (no per-iteration topic), and pre-phase-5 paths
	// keep legacy direct-commit semantics. When empty, the
	// branch name is composed from the Run* + IterSeq fields.
	BranchOverride string

	// Files written + which to stage
	Files         []FileWrite
	ArtifactPaths []string // subset of Files that are user artifact writes

	// Authorship
	Citizen   Identity
	ModelName string // for AI-Model trailer; "" for human-only commits

	// Trailer values (Verdict applies for review tasks)
	Verdict     string // "approve" | "reject" | "request_changes" | ""
	CustomTrailers map[string]string // optional per-call extras

	// Push / retry knobs
	MaxRetries int
}

// SubmitResult is what Workflow.SubmitTaskResult returns.
type SubmitResult struct {
	// CommitSHA of the final commit as it landed on the remote.
	CommitSHA string

	// BranchName is the topic branch this commit landed on, as
	// composed by Conventions.BranchName. Service threads this
	// back to coord for the iteration record.
	BranchName string

	// PushAttempts is the number of push attempts made (1 means
	// FF on first try; >1 means we hit a non-FF and retried).
	PushAttempts int

	// NoOp is true when the would-be commit would have been
	// identical to HEAD (no file changes). Caller's policy
	// whether to treat as success or surface to operator.
	NoOp bool
}

// FileWrite is one file to write to the worktree as part of a
// submission. Aliased to git.FileWrite — same shape, same
// semantics, just exposed in enjugit's namespace so service
// callers don't need to import git directly.
type FileWrite = git.FileWrite

// CommitArbitraryFilesRequest packages inputs for
// Workflow.CommitArbitraryFiles. Used for non-task commits
// like diagram exports, event timeline snapshots, README
// updates — anything that belongs in the project's history
// but isn't a task submission.
type CommitArbitraryFilesRequest struct {
	// Files written to the worktree before staging. Mode 0 → 0644.
	Files []FileWrite

	// Branch the commit lands on. Empty → workflow's default
	// branch.
	Branch string

	// Subject + Body compose the commit message body. Subject is
	// the first line; empty body produces a subject-only commit.
	Subject string
	Body    string

	// AuthorName / AuthorEmail populate the commit author. Empty
	// falls back to the workflow's SystemAuthor.
	AuthorName  string
	AuthorEmail string

	// ModelName, when non-empty, appends an `AI-Model: <x>`
	// trailer + the corresponding Co-Authored-By trailer.
	ModelName string

	// CustomTrailers are additional trailers appended after the
	// canonical Enju-* set. Used for export-type-specific tags.
	CustomTrailers map[string]string
}

// CommitArbitraryFilesResult is what
// Workflow.CommitArbitraryFiles returns.
type CommitArbitraryFilesResult struct {
	CommitSHA string
	NoOp      bool
}

// MergeResult is what Workflow.MergeAcceptedTopic returns.
// Same XxxResult shape as SubmitResult / BatchResult / ScanResult
// — every multi-output verb returns a typed result struct so
// callers don't have to read positional return values to decode
// what happened.
type MergeResult struct {
	// NewTip is the post-merge SHA of the target branch. Whether
	// it's a fast-forward to the topic's tip or a brand-new merge
	// commit depends on FastForwarded.
	NewTip string

	// FastForwarded is true when the merge succeeded as a pure
	// ref move (target had no commits ahead of the topic's fork
	// point). False when a merge commit had to be authored to
	// reconcile diverged history. Surface for telemetry / commit-
	// message rendering — service distinguishes "FF merge" from
	// "merge commit" in the branch_merged event metadata.
	FastForwarded bool
}

// MergeAuthor identifies the actor whose ACCEPT triggered an
// auto-merge. Used for commit author + Enju-Triggered-By trailer.
type MergeAuthor struct {
	// Citizen is the actor whose acceptance triggered this merge.
	// Empty Citizen means "system" (auto-merge during reconcile).
	// Workflow.MergeAcceptedTopic uses this to choose between
	// system author and citizen author per spec.
	Citizen Identity

	// TaskID is the task whose ACCEPT triggered the merge. Goes
	// into the Enju-Triggered-By trailer.
	TaskID string

	// AutoOrManual is "auto" for cascade-driven auto-merges and
	// "manual" for human-driven merge_resolve task submissions.
	// Goes into the Enju-Merge trailer.
	AutoOrManual string
}
