package enjugit

import (
	"log/slog"

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
//   - state_prep.go   — MaterializeUpstreamForReview, StartIterationBranch,
//                       ResumeIterationBranch, WipeIterationWrites,
//                       ResetCleanWorktree
//   - producing.go    — SubmitTaskResult, AutoMergeAcceptedTopic,
//                       CommitTemplateBundle
//   - sync_read.go    — FetchAllRefs, ReadFileAtCommit
//   - lifecycle.go    — EnsureBareRemote, SetRemote
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

	// templateRoots are the configured templates directories,
	// resolved from enju/conf.yaml's `templates:` list (or just
	// `enju/templates/` when no conf). Set by service after
	// Workflow construction. Empty defaults to corelayout's
	// DefaultTemplatesDir.
	templateRoots []string

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

// SetTemplateRoots updates the configured template roots.
// Empty / nil resets to default.
func (w *Workflow) SetTemplateRoots(roots []string) {
	w.templateRoots = roots
}

// TemplateRoots returns the configured roots, falling back
// to the corelayout default when none were set.
func (w *Workflow) TemplateRoots() []string {
	if len(w.templateRoots) > 0 {
		return w.templateRoots
	}
	return nil // ListTemplates fills in DefaultTemplatesDir
}

// WorkDir returns the worktree path for this workflow's clone,
// or "" when the underlying git.Ops implementation doesn't
// expose one (test fakes). Used by callers that need to spawn
// processes in the workdir or stat files there.
func (w *Workflow) WorkDir() string {
	return w.workDir()
}

// ProjectID returns the project ID this Workflow operates on.
// Used by service callers that need to log alongside coord ops.
func (w *Workflow) ProjectID() int64 { return w.projID }

// Conventions returns the conventions this workflow was built
// with. Useful for diagnostic surfaces that want to render the
// same names workflow uses.
func (w *Workflow) Conventions() Conventions { return w.convs }

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
