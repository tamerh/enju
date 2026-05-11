package gitcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- InitEmptyBare ---

func TestInitEmptyBareCreates(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "managed.git")
	if err := InitEmptyBare(bare, "main"); err != nil {
		t.Fatalf("InitEmptyBare: %v", err)
	}
	if !HasBare(bare) {
		t.Error("HasBare returned false after InitEmptyBare")
	}
	// HEAD should symbolic-ref refs/heads/main.
	head, err := os.ReadFile(filepath.Join(bare, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(head), "refs/heads/main") {
		t.Errorf("HEAD = %q, want symbolic ref to refs/heads/main", head)
	}
}

func TestInitEmptyBareDefaultBranchOverride(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "managed.git")
	if err := InitEmptyBare(bare, "trunk"); err != nil {
		t.Fatalf("InitEmptyBare: %v", err)
	}
	head, err := os.ReadFile(filepath.Join(bare, "HEAD"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(head), "refs/heads/trunk") {
		t.Errorf("HEAD = %q, want refs/heads/trunk", head)
	}
}

func TestInitEmptyBareIdempotent(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "managed.git")
	if err := InitEmptyBare(bare, "main"); err != nil {
		t.Fatal(err)
	}
	// Marker file inside the bare to detect if init nukes content.
	marker := filepath.Join(bare, "refs", "heads", "marker-ref-file")
	if err := os.MkdirAll(filepath.Dir(marker), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(marker, []byte("preserved"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := InitEmptyBare(bare, "main"); err != nil {
		t.Errorf("idempotent re-init failed: %v", err)
	}
	got, err := os.ReadFile(marker)
	if err != nil {
		t.Errorf("marker file lost on re-init: %v", err)
	}
	if string(got) != "preserved" {
		t.Errorf("marker contents = %q, want preserved", got)
	}
}

func TestInitEmptyBareEmptyDefaultBranchUsesMain(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "managed.git")
	if err := InitEmptyBare(bare, ""); err != nil {
		t.Fatalf("InitEmptyBare: %v", err)
	}
	head, _ := os.ReadFile(filepath.Join(bare, "HEAD"))
	if !strings.Contains(string(head), "refs/heads/main") {
		t.Errorf("empty defaultBranch should fall back to main; HEAD = %q", head)
	}
}

// --- InitBareWithMirrorFetch ---

func TestInitBareWithMirrorFetchCopiesEveryBranch(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	bare := filepath.Join(tmp, "bare.git")

	gitInit(t, source)
	commitWithMessage(t, source, "a.txt", "x", "first")
	gitRun(t, source, "branch", "topic-x")
	gitRun(t, source, "branch", "topic-y")

	if err := InitBareWithMirrorFetch(bare, source); err != nil {
		t.Fatalf("InitBareWithMirrorFetch: %v", err)
	}

	for _, b := range []string{"main", "topic-x", "topic-y"} {
		out, err := runGit(bare, []string{"rev-parse", "--verify", "refs/heads/" + b}, runOpts{})
		if err != nil {
			t.Errorf("bare missing branch %s: %v", b, err)
		}
		if !isHexSHA(strings.TrimSpace(string(out))) {
			t.Errorf("bare branch %s resolved to non-SHA: %q", b, out)
		}
	}
}

func TestInitBareWithMirrorFetchPreservesSHAs(t *testing.T) {
	tmp := t.TempDir()
	source := filepath.Join(tmp, "source")
	bare := filepath.Join(tmp, "bare.git")

	gitInit(t, source)
	sha := commitWithMessage(t, source, "a.txt", "x", "first")

	if err := InitBareWithMirrorFetch(bare, source); err != nil {
		t.Fatalf("InitBareWithMirrorFetch: %v", err)
	}

	got := strings.TrimSpace(gitRun(t, bare, "rev-parse", "refs/heads/main"))
	if got != sha {
		t.Errorf("bare main = %s, want %s", got, sha)
	}
}

// --- SetOriginOnWorkTree ---

func TestSetOriginOnWorkTreeCreatesWhenMissing(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	if err := SetOriginOnWorkTree(dir, "/path/to/bare"); err != nil {
		t.Fatalf("SetOriginOnWorkTree: %v", err)
	}
	got := strings.TrimSpace(gitRun(t, dir, "remote", "get-url", "origin"))
	if got != "/path/to/bare" {
		t.Errorf("origin URL = %q, want /path/to/bare", got)
	}
}

func TestSetOriginOnWorkTreeReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/old/url")

	if err := SetOriginOnWorkTree(dir, "/new/url"); err != nil {
		t.Fatalf("SetOriginOnWorkTree: %v", err)
	}
	got := strings.TrimSpace(gitRun(t, dir, "remote", "get-url", "origin"))
	if got != "/new/url" {
		t.Errorf("origin URL = %q, want /new/url", got)
	}
}

func TestSetOriginOnWorkTreeIdempotentWhenSame(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/same/url")

	if err := SetOriginOnWorkTree(dir, "/same/url"); err != nil {
		t.Errorf("idempotent call failed: %v", err)
	}
	got := strings.TrimSpace(gitRun(t, dir, "remote", "get-url", "origin"))
	if got != "/same/url" {
		t.Errorf("origin URL = %q, want /same/url", got)
	}
}

// --- HasBare ---

func TestHasBareTrueOnRealBare(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	if !HasBare(bare) {
		t.Error("expected HasBare=true on a real bare repo")
	}
}

func TestHasBareFalseOnEmptyDir(t *testing.T) {
	dir := t.TempDir()
	if HasBare(dir) {
		t.Error("expected HasBare=false on empty dir")
	}
}

func TestHasBareFalseOnNonBareWorktree(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir) // non-bare
	if HasBare(dir) {
		t.Error("expected HasBare=false on non-bare worktree")
	}
}

func TestHasBareFalseOnMissingDir(t *testing.T) {
	if HasBare(filepath.Join(t.TempDir(), "no-such-dir")) {
		t.Error("expected HasBare=false on missing dir")
	}
}

func TestHasBareFalseOnDirWithFakeHEAD(t *testing.T) {
	// A dir with a HEAD file but not actually a git repo.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "HEAD"), []byte("garbage"), 0o644); err != nil {
		t.Fatal(err)
	}
	if HasBare(dir) {
		t.Error("expected HasBare=false on fake-HEAD pathological dir")
	}
}
