package enjugit

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
	"strings"
)

// IsSSHURL returns true when the URL looks like an SSH git remote
// (`git@host:org/repo.git` or `ssh://...`). Pure string classifier,
// no network. Used by callers that need to choose between SSH and
// HTTPS auth before constructing a clone or checking remote state.
func IsSSHURL(url string) bool {
	if strings.HasPrefix(url, "ssh://") {
		return true
	}
	// git@github.com:org/repo.git pattern: contains @ and :, but not ://
	if strings.Contains(url, "@") && strings.Contains(url, ":") && !strings.Contains(url, "://") {
		return true
	}
	return false
}

// IsLocalWorkingTree returns true when path is a directory
// containing a `.git` subdirectory — i.e. a real on-disk git
// working tree (not a bare, not a worktree-link, not nothing).
// Used to detect enju_create_project-adopted projects whose path is stored as
// remote_url on the coordinator.
func IsLocalWorkingTree(path string) bool {
	info, err := os.Stat(path)
	if err != nil || !info.IsDir() {
		return false
	}
	gitDir := filepath.Join(path, ".git")
	gitInfo, err := os.Stat(gitDir)
	return err == nil && gitInfo.IsDir()
}

// FriendlyGitError wraps a raw go-git error with an actionable hint
// based on the operation being performed (clone/push/pull/fetch/
// ls-remote) and a best-effort classification of the underlying cause
// (auth, network, unknown host, non-fast-forward). The original error
// is wrapped with %w so callers can still errors.Is/As against it.
//
// op is a short verb phrase like "clone", "push", "fetch origin" that
// appears at the start of the message. remoteURL is optional; when
// set it's included so the user sees which remote failed.
func FriendlyGitError(op, remoteURL string, err error) error {
	if err == nil {
		return nil
	}
	msg := strings.ToLower(err.Error())
	var hint string
	switch {
	case strings.Contains(msg, "ssh:") ||
		strings.Contains(msg, "handshake failed") ||
		strings.Contains(msg, "publickey") ||
		strings.Contains(msg, "unable to authenticate"):
		hint = "check that your SSH agent has the right key loaded (`ssh-add -l`) and that the key is authorized on the remote"
	case strings.Contains(msg, "authentication required") ||
		strings.Contains(msg, "authorization failed") ||
		strings.Contains(msg, "401") ||
		strings.Contains(msg, "403"):
		hint = "check your git credential helper or ~/.netrc — HTTPS remotes need a valid token/password"
	case strings.Contains(msg, "non-fast-forward") ||
		strings.Contains(msg, "fetch first") ||
		strings.Contains(msg, "rejected"):
		hint = "remote has advanced — run enju_project_sync to refresh, or retry the submit"
	case strings.Contains(msg, "no such host") ||
		strings.Contains(msg, "dial tcp") ||
		strings.Contains(msg, "connection refused") ||
		strings.Contains(msg, "i/o timeout") ||
		strings.Contains(msg, "network is unreachable"):
		hint = "network/DNS issue — check connectivity to the git host"
	case strings.Contains(msg, "repository not found") ||
		strings.Contains(msg, "does not exist"):
		if isLocalGitPath(remoteURL) {
			hint = "verify the path exists and points to a valid bare repository"
		} else {
			hint = "verify the remote URL and that your account has access"
		}
	}
	where := ""
	if remoteURL != "" {
		where = " " + remoteURL
	}
	if hint == "" {
		return fmt.Errorf("%s%s: %w", op, where, err)
	}
	return fmt.Errorf("%s%s: %w (hint: %s)", op, where, err, hint)
}

// isLocalGitPath returns true if remoteURL looks like a local
// filesystem path rather than a network URL. Used by FriendlyGitError
// to pick the right "not found" hint.
func isLocalGitPath(remoteURL string) bool {
	if remoteURL == "" {
		return false
	}
	if strings.Contains(remoteURL, "://") {
		return false
	}
	if i := strings.Index(remoteURL, ":"); i > 0 {
		if strings.Contains(remoteURL[:i], "@") {
			return false
		}
	}
	return true
}

// ArtifactPath returns the repo-relative path for a user-facing
// artifact. Artifacts live at their natural path in the repo root
// (no prefix), so `writes_artifacts: [figures/fig1.png]` writes
// directly to `figures/fig1.png`. Validation (no ../, no .git/,
// no enju/) is the caller's responsibility.
//
// Currently identity — kept as a typed verb so call sites read
// "this is an artifact path, not a workspace-tmp path or
// anything else", and so we have one place to change if the
// layout convention ever evolves.
func ArtifactPath(userPath string) string {
	return userPath
}

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
