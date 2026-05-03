package workspace

// Shared-root helpers for the untracked-artifacts feature.
// When a citizen configures ENJU_SHARED_ROOT, untracked
// artifacts (track:false in YAML) are materialized as
// symlinks from the workspace to a shared path like
// $ENJU_SHARED_ROOT/<project-slug>-<id>/<branch>/<relPath>.
// Both the producer and downstream consumers point at the
// same shared target, so a lab NFS / S3FS / any mountpoint
// the team shares becomes the bytes-transport layer — git
// stays slim, citizens still see each other's outputs.
//
// When ENJU_SHARED_ROOT is unset the helpers are no-ops and
// the workspace path stays a regular file the producer wrote
// directly; sibling citizens won't see it without a shared
// mount, which is the expected local-only behavior.

import (
	"fmt"
	"os"
	"path/filepath"
)

// SharedRootEnv is the env var name carrying the citizen's
// configured shared-storage root. Exposed so the compute
// wrapper can pass it through verbatim into spawned
// scripts' environments (same name → same contract script-
// side as on the fat-client side).
const SharedRootEnv = "ENJU_SHARED_ROOT"

// SharedRoot reads ENJU_SHARED_ROOT from the process
// environment and returns the cleaned absolute path, or
// empty when unset / non-absolute / unreadable. Callers
// that get "" must treat it as "shared root disabled" and
// fall through to the local-only code path.
func SharedRoot() string {
	v := os.Getenv(SharedRootEnv)
	if v == "" {
		return ""
	}
	abs, err := filepath.Abs(v)
	if err != nil {
		return ""
	}
	return abs
}

// SharedArtifactPath builds the canonical shared-storage
// location for one artifact, keyed by (project-slug + id,
// branch, repo-relative path). The slug+id pattern mirrors
// the per-project workspace directory layout so the two
// directory trees stay visually recognizable next to each
// other.
//
// Callers pass empty projectName to fall back to numeric-id
// naming (parity with Workspace.projectDir). Empty branch
// collapses to "main" — same default the artifact index
// uses.
func SharedArtifactPath(shared string, projectID int64, projectName, branch, relPath string) string {
	if shared == "" || relPath == "" {
		return ""
	}
	if branch == "" {
		branch = "main"
	}
	var projectDir string
	if projectName == "" {
		projectDir = fmt.Sprintf("%d", projectID)
	} else {
		slug := slugify(projectName)
		if slug == "" {
			projectDir = fmt.Sprintf("%d", projectID)
		} else {
			projectDir = fmt.Sprintf("%s-%d", slug, projectID)
		}
	}
	return filepath.Join(shared, projectDir, branch, relPath)
}

// EnsureSharedSymlink materializes the workspace-side symlink
// that makes an untracked artifact visible at its repo-relative
// path. Idempotent and defensive:
//
//   - If ENJU_SHARED_ROOT is unset, returns nil without touching
//     disk. Callers continue with local-only behavior.
//   - If the workspace path is already a symlink pointing at the
//     correct shared target, returns nil (the common steady
//     state).
//   - If the workspace path is a symlink pointing at the *wrong*
//     target, the symlink is replaced atomically (unlink + new
//     symlink). Stale symlinks from a reused workspace dir can't
//     mislead the claim-time presence check.
//   - If the workspace path is a regular file (not a symlink),
//     the helper leaves it in place: a citizen who has a local
//     copy via some other means still reads the expected bytes;
//     overwriting would destroy their data.
//   - Always ensures both the shared-target's parent directory
//     and the workspace path's parent directory exist, so the
//     symlink creation never fails on mkdir.
//
// The function does NOT create the file on the shared side —
// that's the producer's job (the script writes through the
// symlink). It only prepares the symlink plumbing so reads
// and writes route to shared storage.
func EnsureSharedSymlink(workspaceRelPath string, workDir string, projectID int64, projectName, branch, relPath string) error {
	shared := SharedRoot()
	if shared == "" {
		return nil
	}
	target := SharedArtifactPath(shared, projectID, projectName, branch, relPath)
	if target == "" {
		return nil
	}

	// Ensure the shared-side parent exists so the script's
	// writes don't explode on "no such file or directory".
	if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
		return fmt.Errorf("creating shared parent %q: %w", filepath.Dir(target), err)
	}

	wsFull := filepath.Join(workDir, workspaceRelPath)
	if err := os.MkdirAll(filepath.Dir(wsFull), 0755); err != nil {
		return fmt.Errorf("creating workspace parent %q: %w", filepath.Dir(wsFull), err)
	}

	// os.Lstat instead of Stat — don't follow the symlink, we
	// want to inspect the link itself.
	fi, err := os.Lstat(wsFull)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("stat %q: %w", wsFull, err)
	}
	if err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			existing, rerr := os.Readlink(wsFull)
			if rerr == nil && existing == target {
				// Already pointing where we want it.
				return nil
			}
			// Wrong symlink — replace. os.Symlink won't
			// overwrite, so unlink first.
			if err := os.Remove(wsFull); err != nil {
				return fmt.Errorf("replacing stale symlink %q: %w", wsFull, err)
			}
		} else {
			// Regular file or directory. Leave alone — the
			// citizen has a local copy; symlinking over it
			// would destroy data. The downstream presence
			// check will stat the existing file and succeed.
			return nil
		}
	}

	if err := os.Symlink(target, wsFull); err != nil {
		return fmt.Errorf("creating symlink %q → %q: %w", wsFull, target, err)
	}
	return nil
}
