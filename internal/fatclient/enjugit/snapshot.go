package enjugit

// snapshot.go — read-side counterpart to
// CommitRunTemplateSnapshot. Materializes a run's frozen tree
// from git history into an on-disk directory, so compute.Run
// can execute its script as a normal subprocess (subprocesses
// can't read from .git/objects/ — they need real files).
//
// Two variants live here:
//
//   - MaterializeRunRepo (per-run, whole tree): writes the
//     entire commit tree at the run branch's tip to one shared
//     on-disk path (typically <project>/.enju/runs/<N>/snapshot/).
//     Used by create_run so all tasks in the run read from one
//     materialized view of both the in-git template snapshot
//     AND the rest of the repo. This is the per-run-snapshot
//     redesign's main verb.
//
//   - MaterializeRunSnapshot (per-task, template subtree only):
//     legacy verb that walks the in-git template-snapshot
//     subdir into a per-task scratch path. Kept for back-compat
//     with the execute path that hasn't been migrated yet and
//     for tests that exercise the per-task race condition the
//     plumbing-commit fix originally addressed.
//
// Critical for concurrent create_run support: each variant
// reads from .git/objects/ (no worktree mutation), so parallel
// runs can all materialize their own branches' trees without
// fighting over the shared operator worktree.

import (
	"fmt"
	"os"
	"path/filepath"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
)

// MaterializeRunRepo writes the entire commit tree at branch's
// tip to targetDir. Walks .git/objects/ directly; never touches
// the shared operator worktree. Safe to call concurrently from
// multiple goroutines targeting DIFFERENT targetDirs.
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
// any path component beginning with ".". Files like
// .gitignore or .github/workflows/* at the repo root will not
// be materialized. The frozen recipe (in-git template
// snapshot) and the rest of the visible repo tree are
// covered — the dotfile skip matches the existing per-task
// path's behavior, so this is not a regression.
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

// MaterializeRunSnapshot writes the run's template-snapshot
// subtree from the named branch's tip into targetDir.
//
// Reads from .git/objects/ via WalkSubtreeBlobsAtCommit — never
// touches the shared worktree. Safe to call concurrently from
// multiple goroutines targeting DIFFERENT targetDirs.
//
// runSeq + runSlug select which run's snapshot subdir is
// materialized (`enju/runs/<seq>-<slug>/template-snapshot/`).
// branch is the run's branch name (origin/<branch> tracking
// counts — LocalBranchHash falls back to origin).
//
// targetDir is created if missing. Existing contents are
// overwritten file-by-file; the verb does NOT clean targetDir
// first, leaving cleanup to the caller (typically the per-task
// scratch lifecycle owns the directory).
//
// Returns the number of files materialized. Zero means the
// snapshot subdir wasn't found in the branch's tree — caller's
// choice whether that's an error.
//
// Errors:
//   - branch can't be resolved to a commit → wrapped error.
//   - filesystem write failure → wrapped error.
func (w *Workflow) MaterializeRunSnapshot(branch string, runSeq int, runSlug, targetDir string) (int, error) {
	if branch == "" {
		return 0, fmt.Errorf("enjugit: MaterializeRunSnapshot: branch required")
	}
	if targetDir == "" {
		return 0, fmt.Errorf("enjugit: MaterializeRunSnapshot: targetDir required")
	}
	sha, err := w.git.LocalBranchHash(branch)
	if err != nil {
		return 0, fmt.Errorf("enjugit: MaterializeRunSnapshot: resolve %s: %w", branch, err)
	}
	if sha == "" {
		return 0, fmt.Errorf("enjugit: MaterializeRunSnapshot: branch %q has no local or origin ref", branch)
	}
	if err := os.MkdirAll(targetDir, 0o755); err != nil {
		return 0, fmt.Errorf("enjugit: MaterializeRunSnapshot: mkdir %s: %w", targetDir, err)
	}
	snapshotSubdir := corelayout.RunTemplateSnapshotDir(runSeq, runSlug)
	count := 0
	walkErr := w.git.WalkSubtreeBlobsAtCommit(sha, snapshotSubdir, func(rel string, mode os.FileMode, content []byte) error {
		full := filepath.Join(targetDir, rel)
		// Ensure parent dirs exist AND are writable. A prior
		// materialization may have chmod'd a parent dir 0555
		// via ChmodSnapshotReadOnly; subsequent re-materialization
		// (re-iteration after request_changes, retries) must
		// re-open it for writing.
		parent := filepath.Dir(full)
		if err := os.MkdirAll(parent, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", parent, err)
		}
		_ = os.Chmod(parent, 0o755) // best-effort; ignore on missing/EPERM
		// Remove the existing file if any. WriteFile on a
		// read-only file (chmod 0444 from a prior pass) returns
		// EACCES — `rm -f` semantics are simpler than chmod-
		// then-write race correctness.
		_ = os.Remove(full)
		// Preserve executable bits from git's mode (matches
		// ReadBundleFiles's mode mapping — 0o100755 → 0o755,
		// else 0o644). Required for script files inside the
		// snapshot to remain runnable after materialization.
		fmode := os.FileMode(0o644)
		if mode&0o111 != 0 {
			fmode = 0o755
		}
		if err := os.WriteFile(full, content, fmode); err != nil {
			return fmt.Errorf("write %s: %w", full, err)
		}
		if err := os.Chmod(full, fmode); err != nil {
			return fmt.Errorf("chmod %s: %w", full, err)
		}
		count++
		return nil
	})
	if walkErr != nil {
		return count, fmt.Errorf("enjugit: MaterializeRunSnapshot: walk %s@%s: %w", snapshotSubdir, sha[:12], walkErr)
	}
	return count, nil
}
