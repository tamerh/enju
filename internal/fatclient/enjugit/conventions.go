package enjugit

import (
	"fmt"
	"path/filepath"
)

// Conventions is the seam for everything that's "Enju policy
// about how to use git." Injected at Workspace construction; same
// instance is reused for every Workflow created from that
// Workspace.
//
// Tests pass synthetic Conventions to pin specific behaviors
// (custom branch naming, custom system author, etc.). Production
// callers use NewProductionConventions().
//
// If you find yourself reaching outside Conventions for a value
// that varies (per-project, per-test, per-future-policy-shift),
// add it here. If the value is genuinely constant for all of
// Enju, leave it as a package-level constant in this package.
type Conventions struct {
	// BranchName composes the topic-branch name for one task
	// iteration. Inputs:
	//   - runSeq: the run's seq within its project (1, 2, ...)
	//   - runSlug: the run's name slugified for filesystem use
	//   - taskDef: the task's definition ID (e.g. "develop_a")
	//   - instanceKey: for_each instance key, "" for singleton tasks
	//   - iterSeq: 1-based attempt counter
	// Output: the full branch name (e.g. "1-build/develop_a/iter-1").
	//
	// Production form encodes runSeq + runSlug as one segment
	// ("1-build") to avoid collisions with the run branch (which
	// might literally be named "main", "1-build", etc.). instanceKey
	// becomes its own path segment when non-empty.
	BranchName BranchNameFn

	// SystemAuthor identifies the "system" actor that authors
	// auto-generated commits (auto-merges, template snapshots).
	// Distinct from any citizen. Production: "Enju System" /
	// "enju-system@localhost".
	SystemAuthor Identity

	// TrailerOrder is the canonical order Enju trailers appear in
	// commit messages. Used by the trailer composer so any commit's
	// trailer block has a stable shape regardless of what fields
	// were set. Unknown trailers (caller-provided custom ones)
	// append after this list.
	TrailerOrder []string

	// DefaultRunBranch is the branch name used when a run doesn't
	// specify one. Production: "main".
	DefaultRunBranch string

	// DiskLayout names the per-project on-disk paths for git
	// infrastructure. Each function takes a projectDir and returns
	// a path. Production layout:
	//   BarePath:          <project>/enju/.bare.git
	//   BotClonePath:      <project>/enju/bots/<bot>/clone
	//   OperatorClonePath: <project>/enju/.clone
	DiskLayout DiskLayout
}

// BranchNameFn composes a topic-branch name. See Conventions.BranchName.
type BranchNameFn func(runSeq int, runSlug, taskDef, instanceKey string, iterSeq int) string

// Identity is a (name, email) pair used to populate git Author
// and Committer fields. Plain struct so tests can construct
// arbitrary identities for assertions.
type Identity struct {
	Name  string
	Email string
}

// DiskLayout is the per-project on-disk path policy. Each field
// is a closure rather than a string so tests can inject layouts
// without mocking filesystem state.
type DiskLayout struct {
	BarePath          func(projectDir string) string
	BotClonePath      func(projectDir, botName string) string
	OperatorClonePath func(projectDir string) string
}

// NewProductionConventions returns the canonical Enju policy.
// Used by service.New() at startup; tests build their own
// Conventions literals.
func NewProductionConventions() Conventions {
	return Conventions{
		BranchName:       productionBranchName,
		SystemAuthor:     Identity{Name: "Enju System", Email: "enju-system@localhost"},
		TrailerOrder:     ProductionTrailerOrder(),
		DefaultRunBranch: "main",
		DiskLayout: DiskLayout{
			BarePath: func(projectDir string) string {
				return filepath.Join(projectDir, "enju", ".bare.git")
			},
			BotClonePath: func(projectDir, botName string) string {
				return filepath.Join(projectDir, "enju", "bots", botName, "clone")
			},
			OperatorClonePath: func(projectDir string) string {
				return filepath.Join(projectDir, "enju", ".clone")
			},
		},
	}
}

// ProductionTrailerOrder is the canonical order Enju trailers
// appear in commit messages. Unknown trailers (custom per-call
// extensions) get appended after this list.
//
// Exposed as a function (not a const slice) so tests that want
// to check ordering can compare against the same source of truth.
func ProductionTrailerOrder() []string {
	return []string{
		// Enju-Task-Complete is the canonical scan key the
		// fetch-path reconciler reads. Order matters: this comes
		// FIRST so a `git log --format='%(trailers)'` quickly
		// shows the task identifier before any metadata.
		TrailerTaskComplete,
		TrailerExit,
		TrailerDurationSeconds,
		TrailerArtifacts,
		TrailerUntrackedArtifacts,
		TrailerIterSeq,
		TrailerVerdict,
		"Enju-Triggered-By",
		"Enju-Merge",
		"Enju-Template-Snapshot",
		"AI-Model",
		"Co-Authored-By",
	}
}

// productionBranchName composes the canonical Enju topic-branch
// name. Format: "<runSeq>-<runSlug>/[<instanceKey>/]<taskDef>/iter-<N>"
//
//   - runSeq + runSlug as one segment ("1-build") to avoid
//     collisions with the run branch literal name.
//   - instanceKey becomes its own path segment when non-empty;
//     for singleton tasks, the segment collapses naturally.
func productionBranchName(runSeq int, runSlug, taskDef, instanceKey string, iterSeq int) string {
	if runSlug == "" {
		runSlug = "run"
	}
	runSegment := fmt.Sprintf("%d-%s", runSeq, runSlug)
	defSegment := taskDef
	if instanceKey != "" {
		defSegment = instanceKey + "/" + taskDef
	}
	return fmt.Sprintf("%s/%s/iter-%d", runSegment, defSegment, iterSeq)
}
