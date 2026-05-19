package enjugit

// snapshot.go — read-side counterpart to
// CommitRunTemplateSnapshot. MaterializeRunRepo writes the run
// branch's whole tree to an on-disk directory so compute.Run
// can execute its script as a normal subprocess (subprocesses
// can't read from .git/objects/ — they need real files).
//
// Reading from .git/objects/ instead of the shared worktree is
// what makes concurrent create_run safe: parallel runs each
// materialize their own branch's tree to their own targetDir,
// no fighting over the operator's single checked-out branch.

import (
	"fmt"
	"os"
	"path/filepath"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
)

// MaterializeRunRepo writes the entire commit tree at branch's
// tip to targetDir. Walks .git/objects/ directly; never touches
// the shared operator worktree.
//
// CONCURRENT WRITES OK: when two replicas of the same bot — or
// any pair of callers — materialize the SAME branch into the
// SAME targetDir simultaneously, both write the same bytes to
// the same paths. The git tree at a SHA is reproducible (same
// SHA → same tree → same blob contents → same on-disk bytes),
// so concurrent writers race onto an idempotent end state.
// Callers targeting DIFFERENT targetDirs are trivially
// independent. Either way, no caller-side coordination needed.
//
// This is the per-run-snapshot variant: ONE call materializes
// everything the run's tasks will read — the in-git
// template-snapshot subdir, plus the rest of the repo at the
// run branch's base SHA. Tasks then resolve scripts via
// $ENJU_TEMPLATE_DIR (a subpath of targetDir) and arbitrary
// repo files via $ENJU_REPO_DIR (= targetDir itself).
//
// branch is the run's branch name. LocalBranchHash resolves
// local refs first and falls back to origin/<branch>.
//
// targetDir is created if missing. Existing files are removed
// before writing (handles a chmod-readonly survivor from a
// prior pass even though the redesign drops chmod enforcement
// — defensive in case a stale snapshot from an older binary is
// still on disk).
//
// Returns the number of files materialized. Errors:
//   - branch can't be resolved → wrapped error.
//   - filesystem write failure → wrapped error.
//
// Every tracked blob is materialized — including dotfiles
// (.gitignore, .mcp.json, .editorconfig, .github/workflows/*,
// .env.example, …) — with ONE bounded-growth exception: the
// per-run result trail force-committed under
// .enju/runs/<seq>-<slug>/<taskDefID>/ is skipped (see
// corelayout.IsRunResultTrailPath). The run branch's tree
// accumulates every prior run's result files; replaying all of
// them into each new run's snapshot made snapshot size grow
// linearly with the project's run count for zero benefit (no
// script reads a prior run's result.md from the frozen
// snapshot). The recipe — .enju/runs/<seq>-<slug>/template-
// snapshot/ — is kept, as is every non-.enju source file.
// Tracking is otherwise the user's decision; the materializer
// doesn't second-guess it. See gitcli.WalkSubtreeBlobsAtCommit
// for the rationale on dropping the old hidden-segment skip.
func (w *Workflow) MaterializeRunRepo(branch, targetDir string) (int, error) {
	if branch == "" {
		return 0, fmt.Errorf("enjugit: MaterializeRunRepo: branch required")
	}
	if targetDir == "" {
		return 0, fmt.Errorf("enjugit: MaterializeRunRepo: targetDir required")
	}
	sha, err := w.git.LocalBranchHash(branch)
	if err != nil {
		return 0, fmt.Errorf("enjugit: MaterializeRunRepo: resolve %s: %w", branch, err)
	}
	if sha == "" {
		return 0, fmt.Errorf("enjugit: MaterializeRunRepo: branch %q has no local or origin ref", branch)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return 0, fmt.Errorf("enjugit: MaterializeRunRepo: mkdir %s: %w", targetDir, err)
	}
	count := 0
	walkErr := w.git.WalkSubtreeBlobsAtCommit(sha, "", func(rel string, mode os.FileMode, content []byte) error {
		// Bounded-snapshot rule: the run branch's tree carries
		// every prior run's force-committed result trail under
		// .enju/runs/<seq>-<slug>/<taskDefID>/. A snapshot only
		// needs the recipe (template-snapshot/) plus the repo's
		// own source — never any run's result trail. Skipping it
		// here keeps each new run's on-disk snapshot bounded by
		// the recipe size instead of growing linearly with the
		// project's cumulative run history.
		if corelayout.IsRunResultTrailPath(filepath.ToSlash(rel)) {
			return nil
		}
		full := filepath.Join(targetDir, rel)
		parent := filepath.Dir(full)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", parent, err)
		}
		_ = os.Remove(full)
		fmode := os.FileMode(0o644)
		if mode&0o111 != 0 {
			fmode = 0o755
		}
		if err := os.WriteFile(full, content, fmode); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}
		count++
		return nil
	})
	if walkErr != nil {
		return count, fmt.Errorf("enjugit: MaterializeRunRepo: walk @%s: %w", sha[:12], walkErr)
	}
	return count, nil
}

