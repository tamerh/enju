package enjugit

import (
	"log/slog"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
)

// Workflow is the mutating handle for one project. Constructed by
// Workspace.ForProject (or its siblings). Wraps a git.Ops with
// the Conventions injected at Workspace construction.
//
// Workflow methods are the surface service depends on. Each verb
// names exactly what it does (in Enju domain terms) and enforces
// pre/post worktree state from the spec. Composition with git
// happens behind the verb — service callers don't touch git
// directly.
//
// Concrete verbs live in:
//
//   - state_prep.go   — EnsureRunBranch, ResetCleanWorktree
//                       (+ unexported iteration-branch lifecycle)
//   - producing.go    — SubmitTaskResult, MergeAcceptedTopic
//   - producing_batch.go — SubmitBatch (multi-task atomic submit)
//   - sync_read.go    — FetchAllRefs, ReadFileAtCommit
//   - lifecycle.go    — EnsureOrigin, SetRemote
//
// Single vs batch submit:
//
//   - SubmitTaskResult: one task → one commit → one push. Use this
//     for the normal claim/submit flow where a citizen finishes a
//     task and lands the result.
//
//   - SubmitBatch: N tasks → N commits → coalesced push (one
//     network round-trip). Use this when the service has multiple
//     tasks ready to submit at once (e.g. enju_execute_run finishing
//     a parallel batch). Atomic: if any commit fails, none of the
//     batch's commits are pushed; partial-batch state is reverted
//     by reseating local branch refs to their pre-batch tips.
//     Single-submit's PushAttempts/MaxRetries semantics don't apply
//     — batch is push-once or fail.
type Workflow struct {
	// git is the plumbing layer. Workflow holds the interface,
	// not the concrete *git.Clone, so tests can pass a fake.
	git git.Ops

	// convs encodes Enju policy. Set at Workspace construction;
	// shared across every Workflow that workspace creates.
	convs Conventions

	// projID identifies which project this Workflow operates on.
	// Used in log lines and (in future) per-project metrics.
	projID int64

	// defaultBranch is the project's default branch (typically
	// "main"). Templates are read from this branch's tree, and
	// templates / submits fork run branches from its tip. Service
	// sets this via SetDefaultBranch after Workflow construction
	// — the coordinator owns the canonical value, Workflow just
	// caches it so verbs don't have to thread it through every
	// call. Empty falls back to convs.DefaultRunBranch.
	defaultBranch string

	logger *slog.Logger
}

// SetDefaultBranch updates the cached default-branch name.
// Service calls this when the coordinator's project record is
// fetched (or when set_project_default_branch fires). Empty
// is a no-op so callers don't have to special-case
// uninitialized state.
func (w *Workflow) SetDefaultBranch(branch string) {
	if branch != "" {
		w.defaultBranch = branch
	}
}

// DefaultBranch returns the configured default branch, or
// the convs-supplied default when none was set explicitly.
func (w *Workflow) DefaultBranch() string {
	if w.defaultBranch != "" {
		return w.defaultBranch
	}
	return w.convs.DefaultRunBranch
}

// WorkDir returns the worktree path for this workflow's clone.
// Delegates to the underlying git.Ops; fakes return whatever
// they're configured with.
func (w *Workflow) WorkDir() string { return w.git.WorkDir() }

// ProjectID returns the project ID this Workflow operates on.
// Used by service callers that need to log alongside coord ops.
func (w *Workflow) ProjectID() int64 { return w.projID }

// RemoteURL returns the on-disk origin URL ("" if no origin
// configured). Used by sync-status surfaces that want to render
// the real git remote alongside the coord-tracked workspace path.
func (w *Workflow) RemoteURL() string { return w.git.RemoteURL() }

// LastPushAt returns the timestamp of the most recent successful
// push from this clone, or zero if none yet. Used by remote-status
// UX.
func (w *Workflow) LastPushAt() time.Time { return w.git.LastPushAt() }

// LastPushError returns the most recent push error string ("" if
// the last push succeeded or none yet). Used by remote-status UX.
func (w *Workflow) LastPushError() string { return w.git.LastPushError() }

// HeadCommitTime returns the author timestamp of HEAD's commit,
// or zero if HEAD is unset / unreadable. Used as a fallback for
// "when was this clone last touched" when no push has happened.
func (w *Workflow) HeadCommitTime() time.Time { return w.git.HeadCommitTime() }

// EnsureOrigin self-heals the on-disk origin remote when something
// (the project package's claim/pull paths) wipes the
// [remote "origin"] section from .git/config. Idempotent.
//
// Band-aid for the dual-handle bug (#381) — see git.Clone.EnsureOrigin
// for the full context. Service callers invoke this after every
// OpenWorkflow so subsequent fetch/push find origin on disk.
func (w *Workflow) EnsureOrigin(url string) error {
	return translateGitError("ensure origin", w.git.EnsureOrigin(url))
}

// SetRemote points the clone at a new origin URL. Empty URL means
// "remove origin entirely" (turning a remote-backed clone into a
// local-only one); a non-empty URL adds or replaces the existing
// origin. Idempotent on both paths.
func (w *Workflow) SetRemote(url string) error {
	if url == "" {
		return translateGitError("remove origin", w.git.RemoveOrigin())
	}
	return translateGitError("set remote", w.git.EnsureOrigin(url))
}

// LocalBranches returns the names of every refs/heads/<name> on
// disk. Used by callers that need to enumerate branches for
// per-branch operations (cursor reset, batch reconcile, etc).
func (w *Workflow) LocalBranches() ([]string, error) {
	branches, err := w.git.LocalBranches()
	return branches, translateGitError("local branches", err)
}

// PushAllRefs ships every local branch to origin in one network
// round-trip via `refs/heads/*:refs/heads/*`. Used by
// enju_set_project_remote to seed a freshly-pointed bare with the
// project's full branch state. Idempotent.
func (w *Workflow) PushAllRefs(force bool) error {
	return translateGitError("push all refs", w.git.PushAllRefs(force))
}
