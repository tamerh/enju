package enjugit

import (
	"path/filepath"
	"testing"
)

// TestResolveBigfilesDir_LocalDefault pins the local-mode path:
// when ENJU_BIGFILES is unset, the bigfiles dir lives under
// .enju/ in the project tree. Living under the .enju/ umbrella
// is what keeps the data gitignored — the operator's project
// tree IS the worktree post-Phase-8.
func TestResolveBigfilesDir_LocalDefault(t *testing.T) {
	t.Setenv(BigfilesEnv, "")
	got := ResolveBigfilesDir("/work/proj", 7, "myproj", "main")
	want := filepath.Join("/work/proj", ".enju", "bigfiles", "main")
	if got != want {
		t.Errorf("local default: got %q, want %q", got, want)
	}
}

// TestResolveBigfilesDir_BranchSubdir pins per-branch isolation —
// parallel-branch runs MUST land in distinct dirs or they'd
// clobber each other's outputs at the same logical path.
func TestResolveBigfilesDir_BranchSubdir(t *testing.T) {
	t.Setenv(BigfilesEnv, "")
	a := ResolveBigfilesDir("/work/proj", 1, "proj", "main")
	b := ResolveBigfilesDir("/work/proj", 1, "proj", "feature-x")
	if a == b {
		t.Errorf("parallel-branch isolation lost: both branches resolved to %q", a)
	}
}

// TestResolveBigfilesDir_EmptyBranchDefaultsToMain matches the
// rest of the system's "" → "main" default. Any path resolution
// that diverges from this would silently route a no-branch
// caller to a different dir than the topic-branch / artifact-
// index paths.
func TestResolveBigfilesDir_EmptyBranchDefaultsToMain(t *testing.T) {
	t.Setenv(BigfilesEnv, "")
	got := ResolveBigfilesDir("/work/proj", 1, "p", "")
	want := ResolveBigfilesDir("/work/proj", 1, "p", "main")
	if got != want {
		t.Errorf("empty branch should equal main: got %q, want %q", got, want)
	}
}

// TestResolveBigfilesDir_SharedRoot covers the cluster case:
// when a citizen sets ENJU_BIGFILES to a shared mount, the
// bigfiles for every project they touch land under that mount,
// per-project + per-branch sharded so multiple projects on the
// same NFS don't collide.
func TestResolveBigfilesDir_SharedRoot(t *testing.T) {
	t.Setenv(BigfilesEnv, "/mnt/shared")
	got := ResolveBigfilesDir("/work/proj", 7, "myproj", "main")
	want := filepath.Join("/mnt/shared", "myproj-7", "main")
	if got != want {
		t.Errorf("shared root: got %q, want %q", got, want)
	}
}

// TestResolveBigfilesDir_SharedRootIgnoresProjectRoot — when the
// shared mount is set, the local projectRoot is irrelevant. The
// caller can pass anything (or nothing) and the result still
// points at the shared mount. Documents that ENJU_BIGFILES is
// the override, not a hint combined with the local path.
func TestResolveBigfilesDir_SharedRootIgnoresProjectRoot(t *testing.T) {
	t.Setenv(BigfilesEnv, "/mnt/shared")
	withRoot := ResolveBigfilesDir("/work/proj", 7, "myproj", "main")
	noRoot := ResolveBigfilesDir("", 7, "myproj", "main")
	if withRoot != noRoot {
		t.Errorf("shared mode should ignore projectRoot: with=%q, without=%q",
			withRoot, noRoot)
	}
}

// TestResolveBigfilesDir_NumericIDFallback documents the
// projectName="" branch — uses the numeric id as the segment.
// Matches Workspace.projectDir's fallback so the bigfiles dir
// for a no-name project sits next to (instead of inside) the
// matching project's workspace dir on the shared mount.
func TestResolveBigfilesDir_NumericIDFallback(t *testing.T) {
	t.Setenv(BigfilesEnv, "/mnt/shared")
	got := ResolveBigfilesDir("/work/proj", 42, "", "main")
	want := filepath.Join("/mnt/shared", "42", "main")
	if got != want {
		t.Errorf("numeric fallback: got %q, want %q", got, want)
	}
}
