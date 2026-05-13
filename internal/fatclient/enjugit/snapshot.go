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
// Note on hidden segments: WalkSubtreeBlobsAtCommit skips
// dot-prefixed segments (.git, .DS_Store, .gitignore,
// .github/workflows/*) EXCEPT for the snapshot-visible allowlist
// — currently just `.enju/`. Enju's audit trail and committed
// template-snapshot live under `.enju/runs/` (post-Phase-8.h
// move from the old visible `enju/runs/`), and skipping them
// would leave the executor unable to find the workflow YAML or
// per-task result history. The rest of the dot-prefix skip
// stands.
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

