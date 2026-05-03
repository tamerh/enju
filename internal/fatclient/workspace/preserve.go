package workspace

// Rename-based preservation of non-tracked files across a
// Force checkout. Workspaces for bio/ML workflows routinely
// hold multi-GB untracked artifacts (BAM files, model
// checkpoints, FASTQ data produced by writes_artifacts
// tracked=false compute tasks). The naive approach — read
// every non-tracked file into memory before the checkout,
// write back after — OOMs on those workloads.
//
// The rename approach moves files out of the way by updating
// the filesystem directory entry only. Bytes never leave the
// disk, so a 50 GB file moves in microseconds and total memory
// stays flat regardless of workspace size.
//
// Invariants:
//
//   - The preserve dir lives as a sibling of workDir
//     (<workDir>.preserve-in-progress), guaranteeing same-
//     filesystem so os.Rename works without EXDEV.
//
//   - For a directory with no tracked descendants (e.g. an
//     entire gitignored data/ tree), we rename the whole
//     subtree in one syscall. This is the big win for the
//     bio-workload case.
//
//   - For a mixed directory (some tracked, some not), we
//     recurse and rename individual non-tracked files. Tracked
//     paths stay in place so Force checkout can do its normal
//     tree swap.
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
//     crash is drained back into workDir using the same
//     rename-if-no-conflict logic as the normal restore.
//
// Known limitations (acceptable v1, file follow-ups if needed):
//
//   - Staged-but-uncommitted changes are in the index, so
//     we classify them as tracked → Force checkout will
//     update/remove them as part of the branch swap. Rare
//     in the fat-client flow (we don't leave staged state),
//     but callers with external tooling that stages could
//     lose staged work. Documented here, not protected.
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
	"strings"

	gogit "github.com/go-git/go-git/v5"
)

// preserveDirSuffix is the name of the sibling directory we
// rename non-tracked workDir entries into during a Force
// checkout. Kept verbose ("-in-progress") so a human finding
// one in the filesystem immediately understands it's a
// transient state, not a backup.
const preserveDirSuffix = ".preserve-in-progress"

// preservedEntry records one rename we performed during
// preservation. Restore iterates this manifest to rename each
// entry back into workDir. isDir distinguishes wholesale-
// subtree preservations (where the whole dir moved in one
// rename) from per-file preservations (where a tracked sibling
// kept us from moving the parent).
type preservedEntry struct {
	relPath string
	isDir   bool
}

// movePreserveNonTracked scans the git index, then walks
// workDir top-down and renames every non-tracked path into
// preserveDir. Returns a manifest of what was moved so the
// caller can restore after its Force checkout.
//
// Top-down walk with subtree short-circuit: when we enter a
// directory that has no tracked descendants, we rename the
// whole subtree in one syscall and skip traversal. This is
// O(1) regardless of subtree size, which is what makes
// multi-GB gitignored artifact dirs safe.
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
	// go-git emits the index in sorted order, but a sort here
	// is defensive — the prefix-check logic below depends on
	// sortedness for correctness, and a cheap sort beats a
	// subtle ordering bug.
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
			// corrupt the repo. Must skip before any other
			// check. Also guards against the preserve dir
			// itself being a sibling of workDir rather than
			// inside it — we never walk into it.
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			if !hasTrackedDescendants(tracked, rel) {
				if err := movePath(workDir, preserveDir, rel); err != nil {
					return fmt.Errorf("preserving directory %q: %w", rel, err)
				}
				manifest = append(manifest, preservedEntry{relPath: rel, isDir: true})
				// Children moved with the parent — don't walk in.
				return filepath.SkipDir
			}
			// Mixed dir: some descendants are tracked. Recurse
			// and handle files individually.
			return nil
		}

		// Regular files and symlinks both go through rename.
		// Sockets/fifos/devices are skipped — they don't fit
		// the data-preservation model (a socket has no stable
		// bytes to preserve; renaming it just moves the
		// endpoint), and are virtually unheard of in workspaces.
		if !info.Mode().IsRegular() && info.Mode()&os.ModeSymlink == 0 {
			return nil
		}

		if isTrackedPath(tracked, rel) {
			return nil
		}
		if err := movePath(workDir, preserveDir, rel); err != nil {
			return fmt.Errorf("preserving file %q: %w", rel, err)
		}
		manifest = append(manifest, preservedEntry{relPath: rel, isDir: false})
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
			// Leaving the preserved copy lets the user
			// diff/merge manually. We don't overwrite branch
			// content.
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

// countConflictFiles totals the regular files stranded in the
// preserve dir across all conflicted manifest entries. A
// wholesale-dir conflict counts every file underneath, not
// just the entry itself — otherwise a 50-file gitignored
// subtree that couldn't be restored would log as
// "conflict_count: 1" and understate the actual divergence.
func countConflictFiles(preserveDir string, conflicts []preservedEntry) int {
	total := 0
	for _, e := range conflicts {
		if !e.isDir {
			total++
			continue
		}
		root := filepath.Join(preserveDir, filepath.FromSlash(e.relPath))
		_ = filepath.Walk(root, func(_ string, info os.FileInfo, werr error) error {
			if werr != nil || info == nil {
				return nil
			}
			if info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
				total++
			}
			return nil
		})
	}
	return total
}

// recoverLeftoverPreserve drains a preserve dir left behind by
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
func recoverLeftoverPreserve(workDir string, logger *slog.Logger) error {
	preserveDir := workDir + preserveDirSuffix
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

// hasTrackedDescendants returns true if any path in `tracked`
// starts with `dir + "/"`. Uses sort.SearchStrings on the
// pre-sorted tracked slice for O(log N) per query.
//
// The root case (dir == "") returns true whenever anything is
// tracked — a whole-workspace wholesale preserve would only
// make sense for an empty repo, and we never call this with
// dir == "" in the walk anyway (workDir itself isn't a candidate).
func hasTrackedDescendants(tracked []string, dir string) bool {
	if dir == "" {
		return len(tracked) > 0
	}
	needle := dir + "/"
	i := sort.SearchStrings(tracked, needle)
	return i < len(tracked) && strings.HasPrefix(tracked[i], needle)
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
