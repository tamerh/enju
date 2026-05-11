package compute

// Snapshot read-only enforcement on the host. Compute scripts run
// with the run's template-snapshot as their working directory; the
// snapshot is canonical and immutable across iterations, but on
// the host-exec path nothing OS-level prevents a buggy script
// from writing to it. (Inside a container the :ro bind mount is
// the strong guarantee; see container_args.go.)
//
// chmod 0444 (files) / 0555 (dirs) is the host-side defense.
// Python __pycache__ writes, shell `cmd > foo.txt` with relative
// paths, tool-specific cache dirs (pytest, cargo, npm) — all of
// these silently fail without polluting the snapshot, surfacing
// EACCES that the calling tool either catches or fails loudly on.
//
// The chmod runs at execute time, not snapshot-creation time:
// git checkout would reset permissions to umask defaults whenever
// the workspace is updated, so a one-shot chmod at creation
// doesn't survive normal operation. Re-chmod'ing files that are
// already read-only is cheap and idempotent.

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

// ChmodSnapshotReadOnly walks the snapshot tree rooted at `root`
// and sets every file to 0444 and every dir to 0555. Symlinks are
// left untouched — chmoding through a symlink would resolve to
// the target, potentially modifying files outside the snapshot
// boundary, which is a containment break, not just a hygiene
// issue.
//
// Returns the first chmod error encountered (rare in practice;
// the bot owns the tree, so EPERM shouldn't happen). Missing
// root is not an error — callers may invoke this before the
// snapshot has been materialized for some reason; the missing
// path produces a no-op.
func ChmodSnapshotReadOnly(root string) error {
	if root == "" {
		return nil
	}
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return nil
	}
	return filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		// Symlinks: leave alone. os.Chmod follows symlinks on
		// Unix, so applying it to a symlink would set the mode
		// on the link's target — which may live outside the
		// snapshot, breaking containment. Skipping symlinks
		// also matches filepath.WalkDir's descent behavior
		// (it does not follow symlinks for traversal), so the
		// walk stays scoped to files actually under `root`.
		if d.Type()&fs.ModeSymlink != 0 {
			return nil
		}
		mode := os.FileMode(0o444)
		if d.IsDir() {
			mode = 0o555
		}
		if cerr := os.Chmod(p, mode); cerr != nil {
			return fmt.Errorf("chmod %s: %w", p, cerr)
		}
		return nil
	})
}
