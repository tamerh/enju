package git

// Rename-based preservation of non-tracked files across a
// Force checkout. go-git's Force checkout (HardReset) walks
// the worktree and deletes anything not in the target tree —
// including untracked files. CLI git keeps them; go-git does
// not. So before every Force checkout we move non-tracked
// paths out of the way, then move them back after. The user's
// scratch files (logs, ad-hoc edits, gitignored configs) survive
// the branch swap.
//
// The rename approach moves files via directory-entry update
// only. Bytes never leave disk, so even a multi-GB scratch
// file moves in microseconds and total memory stays flat.
//
// Invariants:
//
//   - The preserve dir lives as a sibling of workDir
//     (<workDir>.preserve-in-progress), guaranteeing same-
//     filesystem so os.Rename works without EXDEV.
//
//   - The walk is per-file: every non-tracked path becomes
//     its own manifest entry. Empty directories are not
//     preserved — git ignores them and so do we.
//
//   - On any rename failure mid-preservation, we stop
//     immediately and return the partial manifest + an error.
//     The caller (CheckoutBranch) MUST NOT proceed with the
//     actual Force checkout in that case. The preserve dir is
//     left on disk; recovery happens at next workspace open.
//
//   - Restore walks the manifest and renames entries back. If
//     a target path already exists (the new branch tracks it),
//     we leave the preserved copy in the preserve dir rather
//     than overwriting branch content. A preserve dir that's
//     non-empty after restore signals the user that manual
//     reconciliation is needed.
//
//   - Crash recovery at workspace open: a leftover
//     <workDir>.preserve-in-progress from a previous process's
//     crash is drained back into workDir using rename-if-no-
//     conflict logic.
//
// Known limitations:
//
//   - Staged-but-uncommitted changes are in the index, so
//     we classify them as tracked → Force checkout will
//     update/remove them as part of the branch swap.
//     Documented, not protected.
//
//   - Case-insensitive filesystems (default macOS APFS)
//     use exact-case match for tracked-path lookup; a
//     FOO.TXT on disk vs foo.txt in the index would be
//     treated as non-tracked and moved unnecessarily. No
//     corruption, just an extra rename round-trip.

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"

	gogit "github.com/go-git/go-git/v5"
)

// PreserveDirSuffix is the name of the sibling directory we
// rename non-tracked workDir entries into during a Force
// checkout. Kept verbose ("-in-progress") so a human finding
// one in the filesystem immediately understands it's a
// transient state, not a backup.
const PreserveDirSuffix = ".preserve-in-progress"

// preservedEntry records one rename we performed during
// preservation. Restore iterates this manifest to rename each
// entry back into workDir.
type preservedEntry struct {
	relPath string
}

// movePreserveNonTracked scans the git index, walks workDir,
// and renames every non-tracked file (or symlink) into
// preserveDir. Returns a manifest of what was moved so the
// caller can restore after its Force checkout.
//
// Walks per-file. Empty directories are not preserved — git
// doesn't track them and neither do we.
//
// On any rename failure, we return the partial manifest + the
// error. The caller MUST NOT proceed with the Force checkout
// in that case — the workspace is in a split state (some
// non-tracked entries moved, others still in workDir), but
// git state is untouched, so restore (or crash recovery) can
// put things back.
func movePreserveNonTracked(repo *gogit.Repository, workDir, preserveDir string) ([]preservedEntry, error) {
	idx, err := repo.Storer.Index()
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}
	tracked := make([]string, 0, len(idx.Entries))
	for _, e := range idx.Entries {
		tracked = append(tracked, e.Name)
	}
	sort.Strings(tracked)

	var manifest []preservedEntry
	walkErr := filepath.Walk(workDir, func(path string, info os.FileInfo, werr error) error {
		if werr != nil {
			return werr
		}
		if path == workDir {
			return nil
		}
		relOS, rerr := filepath.Rel(workDir, path)
		if rerr != nil {
			return rerr
		}
		rel := filepath.ToSlash(relOS)

		if info.IsDir() {
			// .git/ is the git object store; touching it would
			// corrupt the repo.
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}

		// Regular files and symlinks go through rename.
		// Sockets/fifos/devices are skipped.
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}

		if isTrackedPath(tracked, rel) {
			return nil
		}
		if err := movePath(workDir, preserveDir, rel); err != nil {
			return fmt.Errorf("preserving file %q: %w", rel, err)
		}
		manifest = append(manifest, preservedEntry{relPath: rel})
		return nil
	})
	if walkErr != nil {
		return manifest, walkErr
	}
	return manifest, nil
}

// restoreFromPreserve renames every entry in the manifest back
// into workDir. A target path that already exists after the
// Force checkout means the new branch tracks it — we leave the
// preserved copy in preserveDir rather than overwriting branch
// content, and surface the skipped paths so the caller can warn
// the user.
//
// On success, the preserveDir is cleaned up. If any entries
// remain (because they collided with tracked paths on the new
// branch), the preserveDir stays on disk for manual review.
func restoreFromPreserve(workDir, preserveDir string, manifest []preservedEntry) (conflicts []preservedEntry, err error) {
	for _, e := range manifest {
		src := filepath.Join(preserveDir, filepath.FromSlash(e.relPath))
		dst := filepath.Join(workDir, filepath.FromSlash(e.relPath))
		if _, statErr := os.Lstat(dst); statErr == nil {
			// Target exists — new branch tracks this path.
			// If the bytes match what we preserved, the tree
			// rewrote our user's content identically (typical
			// when the user just submitted those exact files
			// and we're now seeing them via the new branch's
			// tree); silently drop the preserved copy rather
			// than flagging a phantom conflict that would
			// pile up across iterations. Different bytes mean
			// real divergence — keep the preserved copy in
			// the preserve dir for manual review.
			if same, _ := filesEqual(src, dst); same {
				_ = os.Remove(src)
				continue
			}
			conflicts = append(conflicts, e)
			continue
		} else if !os.IsNotExist(statErr) {
			return conflicts, fmt.Errorf("checking target %q: %w", e.relPath, statErr)
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
			return conflicts, fmt.Errorf("restoring %q: creating parent: %w", e.relPath, err)
		}
		if err := os.Rename(src, dst); err != nil {
			return conflicts, fmt.Errorf("restoring %q: %w", e.relPath, err)
		}
	}
	// Sweep empty dirs. Non-empty preserveDir (conflicts) is
	// left as-is for manual review.
	_, _ = pruneEmptyDirs(preserveDir)
	return conflicts, nil
}

// RecoverLeftoverPreserve drains a preserve dir left behind by
// a previous process's crash. Called once on workspace open.
// Uses the same rename-if-no-conflict logic as restoreFromPreserve,
// but without a manifest — walks the preserve dir and mirrors
// its contents back into workDir.
//
// A leftover preserve dir indicates we were mid-checkout when
// the process died. The non-tracked files are still on disk
// (at the preserve dir), but their original positions in
// workDir may or may not exist depending on whether the
// crashed process reached the actual Force checkout:
//
//   - crashed during movePreserveNonTracked → workDir has
//     partial tree, preserve dir has partial tree, recovery
//     reunites them
//
//   - crashed between move and checkout → workDir missing
//     the non-tracked paths, preserve dir has them,
//     recovery restores them
//
//   - crashed between checkout and restore → workDir now on
//     the new branch (tracked files only), preserve dir has
//     the user's non-tracked stuff, recovery restores
//     anything that doesn't collide with new-branch tracked
//     files
//
// All three cases are handled by "walk preserve dir, for each
// entry: rename back if target missing, leave otherwise."
func RecoverLeftoverPreserve(workDir string, logger *slog.Logger) error {
	preserveDir := workDir + PreserveDirSuffix
	info, err := os.Stat(preserveDir)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.IsDir() {
		// Not a dir — something else with that exact name.
		// Bail without touching it.
		return nil
	}
	if logger != nil {
		logger.Warn("found leftover preserve dir from previous crash, attempting recovery",
			"path", preserveDir)
	}
	if err := recoverWalk(workDir, preserveDir, ""); err != nil {
		return fmt.Errorf("recovering %s: %w", preserveDir, err)
	}
	_, _ = pruneEmptyDirs(preserveDir)
	if logger != nil {
		if _, err := os.Stat(preserveDir); err == nil {
			logger.Warn("preserve dir still contains conflicts after recovery — manual review needed",
				"path", preserveDir)
		} else {
			logger.Info("preserve dir recovery complete")
		}
	}
	return nil
}

// recoverWalk is the recursive helper for crash recovery.
// Walks the preserve dir, for each entry:
//   - target missing: rename back (wholesale for dirs, per-file
//     otherwise)
//   - target exists: recurse (for dirs — mixed content) or
//     leave in preserve (for files — conflict)
func recoverWalk(workDir, preserveDir, rel string) error {
	src := filepath.Join(preserveDir, filepath.FromSlash(rel))
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		entryRel := rel
		if entryRel != "" {
			entryRel += "/"
		}
		entryRel += e.Name()
		srcPath := filepath.Join(preserveDir, filepath.FromSlash(entryRel))
		dstPath := filepath.Join(workDir, filepath.FromSlash(entryRel))

		if e.IsDir() {
			if _, statErr := os.Lstat(dstPath); statErr == nil {
				// workDir has a dir here (tracked on current
				// branch). Recurse and handle children
				// individually — some may be restorable, some
				// may conflict with tracked siblings.
				if err := recoverWalk(workDir, preserveDir, entryRel); err != nil {
					return err
				}
				continue
			}
			// Target absent — rename the whole subtree back.
			if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
				return err
			}
			if err := os.Rename(srcPath, dstPath); err != nil {
				return fmt.Errorf("restoring %q: %w", entryRel, err)
			}
			continue
		}
		// File or symlink.
		if _, statErr := os.Lstat(dstPath); statErr == nil {
			// Conflict — leave in preserve dir.
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dstPath), 0755); err != nil {
			return err
		}
		if err := os.Rename(srcPath, dstPath); err != nil {
			return fmt.Errorf("restoring %q: %w", entryRel, err)
		}
	}
	return nil
}

// movePath renames workDir/rel → preserveDir/rel, creating
// parent dirs in the destination as needed.
func movePath(workDir, preserveDir, rel string) error {
	src := filepath.Join(workDir, filepath.FromSlash(rel))
	dst := filepath.Join(preserveDir, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(dst), 0755); err != nil {
		return fmt.Errorf("creating preserve parent for %q: %w", rel, err)
	}
	if err := os.Rename(src, dst); err != nil {
		return fmt.Errorf("renaming %q into preserve dir: %w", rel, err)
	}
	return nil
}

// isTrackedPath returns true if `path` is an exact match for
// some entry in the sorted tracked slice.
func isTrackedPath(tracked []string, path string) bool {
	i := sort.SearchStrings(tracked, path)
	return i < len(tracked) && tracked[i] == path
}

// pruneEmptyDirs removes empty directories bottom-up from
// root. Returns (true, nil) if root was itself removed, (false,
// nil) if it still holds entries. Best-effort — any error
// during walk short-circuits without propagating, because
// cleanup failure shouldn't mask a successful restore.
func pruneEmptyDirs(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return true, nil
		}
		return false, err
	}
	allEmpty := true
	for _, e := range entries {
		if !e.IsDir() {
			allEmpty = false
			continue
		}
		sub := filepath.Join(root, e.Name())
		empty, err := pruneEmptyDirs(sub)
		if err != nil {
			allEmpty = false
			continue
		}
		if !empty {
			allEmpty = false
		}
	}
	if allEmpty {
		if err := os.Remove(root); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// filesEqual returns true when both paths exist as regular files
// with identical contents. Any error (missing file, permission,
// non-regular type, mismatched size or bytes) yields false. Used
// by restoreFromPreserve to recognise "the new branch's tree
// just rewrote the bytes we saved" so we drop a redundant
// preserved copy instead of treating it as a real conflict.
func filesEqual(a, b string) (bool, error) {
	infoA, err := os.Lstat(a)
	if err != nil {
		return false, err
	}
	infoB, err := os.Lstat(b)
	if err != nil {
		return false, err
	}
	if !infoA.Mode().IsRegular() || !infoB.Mode().IsRegular() {
		return false, nil
	}
	if infoA.Size() != infoB.Size() {
		return false, nil
	}
	bytesA, err := os.ReadFile(a)
	if err != nil {
		return false, err
	}
	bytesB, err := os.ReadFile(b)
	if err != nil {
		return false, err
	}
	if len(bytesA) != len(bytesB) {
		return false, nil
	}
	for i := range bytesA {
		if bytesA[i] != bytesB[i] {
			return false, nil
		}
	}
	return true, nil
}
