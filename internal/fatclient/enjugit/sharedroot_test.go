package enjugit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSharedRootEnvReading(t *testing.T) {
	t.Setenv(SharedRootEnv, "")
	if got := SharedRoot(); got != "" {
		t.Errorf("unset should return \"\", got %q", got)
	}

	// Absolute path survives cleaning.
	t.Setenv(SharedRootEnv, "/mnt/enju/")
	if got := SharedRoot(); got != "/mnt/enju" {
		t.Errorf("absolute: got %q, want /mnt/enju", got)
	}

	// Relative path gets resolved. Just check that the
	// returned path is absolute — the exact working-dir
	// depends on the test runner.
	t.Setenv(SharedRootEnv, "relative-dir")
	got := SharedRoot()
	if got == "" || !filepath.IsAbs(got) {
		t.Errorf("relative path should resolve to abs, got %q", got)
	}
}

func TestSharedArtifactPathSlug(t *testing.T) {
	got := SharedArtifactPath("/mnt/enju", 7, "Battle Test", "main", "out/big.bam")
	want := "/mnt/enju/battle-test-7/main/out/big.bam"
	if got != want {
		t.Errorf("slug+id layout wrong: got %q want %q", got, want)
	}
}

func TestSharedArtifactPathNumericFallback(t *testing.T) {
	got := SharedArtifactPath("/mnt/enju", 42, "", "main", "out/x")
	want := "/mnt/enju/42/main/out/x"
	if got != want {
		t.Errorf("numeric fallback wrong: got %q want %q", got, want)
	}
}

func TestSharedArtifactPathBranchDefault(t *testing.T) {
	got := SharedArtifactPath("/mnt/enju", 1, "demo", "", "out/x")
	if !strings.Contains(got, "/main/") {
		t.Errorf("empty branch should default to main, got %q", got)
	}
}

func TestSharedArtifactPathEmptyInputs(t *testing.T) {
	if got := SharedArtifactPath("", 1, "demo", "main", "out/x"); got != "" {
		t.Errorf("empty shared → empty path, got %q", got)
	}
	if got := SharedArtifactPath("/m", 1, "demo", "main", ""); got != "" {
		t.Errorf("empty relPath → empty path, got %q", got)
	}
}

func TestEnsureSharedSymlinkUnsetIsNoop(t *testing.T) {
	t.Setenv(SharedRootEnv, "")
	workDir := t.TempDir()
	if err := EnsureSharedSymlink("out/x", workDir, 1, "demo", "main", "out/x"); err != nil {
		t.Fatalf("unset shared should not error: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(workDir, "out/x")); !os.IsNotExist(err) {
		t.Errorf("should not have created anything, got err=%v", err)
	}
}

func TestEnsureSharedSymlinkCreatesSymlink(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(SharedRootEnv, shared)
	workDir := t.TempDir()

	err := EnsureSharedSymlink("out/big.bam", workDir, 7, "demo", "main", "out/big.bam")
	if err != nil {
		t.Fatal(err)
	}
	wsPath := filepath.Join(workDir, "out/big.bam")
	fi, err := os.Lstat(wsPath)
	if err != nil {
		t.Fatalf("expected symlink at %q, err=%v", wsPath, err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("expected symlink, got mode=%v", fi.Mode())
	}
	target, err := os.Readlink(wsPath)
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(shared, "demo-7", "main")
	if !strings.HasPrefix(target, wantPrefix) {
		t.Errorf("symlink target %q should be under %q", target, wantPrefix)
	}
}

func TestEnsureSharedSymlinkIdempotent(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(SharedRootEnv, shared)
	workDir := t.TempDir()

	if err := EnsureSharedSymlink("out/x", workDir, 1, "demo", "main", "out/x"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.Readlink(filepath.Join(workDir, "out/x"))

	if err := EnsureSharedSymlink("out/x", workDir, 1, "demo", "main", "out/x"); err != nil {
		t.Fatal(err)
	}
	after, _ := os.Readlink(filepath.Join(workDir, "out/x"))
	if before != after {
		t.Errorf("idempotent call changed target: %q → %q", before, after)
	}
}

func TestEnsureSharedSymlinkReplacesStale(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(SharedRootEnv, shared)
	workDir := t.TempDir()

	wsPath := filepath.Join(workDir, "out/x")
	_ = os.MkdirAll(filepath.Dir(wsPath), 0755)
	_ = os.Symlink("/some/old/path", wsPath)

	if err := EnsureSharedSymlink("out/x", workDir, 1, "demo", "main", "out/x"); err != nil {
		t.Fatal(err)
	}
	target, _ := os.Readlink(wsPath)
	if target == "/some/old/path" {
		t.Errorf("stale symlink not replaced, still points at %q", target)
	}
	want := SharedArtifactPath(shared, 1, "demo", "main", "out/x")
	if target != want {
		t.Errorf("new target wrong: got %q want %q", target, want)
	}
}

func TestEnsureSharedSymlinkPreservesRegularFile(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(SharedRootEnv, shared)
	workDir := t.TempDir()

	wsPath := filepath.Join(workDir, "out/x")
	_ = os.MkdirAll(filepath.Dir(wsPath), 0755)
	if err := os.WriteFile(wsPath, []byte("local bytes"), 0644); err != nil {
		t.Fatal(err)
	}

	if err := EnsureSharedSymlink("out/x", workDir, 1, "demo", "main", "out/x"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Lstat(wsPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		t.Error("regular file was clobbered with a symlink")
	}
	data, _ := os.ReadFile(wsPath)
	if string(data) != "local bytes" {
		t.Errorf("content lost: got %q", string(data))
	}
}

func TestEnsureSharedSymlinkCreatesSharedParent(t *testing.T) {
	shared := t.TempDir()
	t.Setenv(SharedRootEnv, shared)
	workDir := t.TempDir()

	if err := EnsureSharedSymlink("deep/nested/x", workDir, 5, "foo", "feature-br", "deep/nested/x"); err != nil {
		t.Fatal(err)
	}
	sharedDir := filepath.Join(shared, "foo-5", "feature-br", "deep", "nested")
	if fi, err := os.Stat(sharedDir); err != nil || !fi.IsDir() {
		t.Errorf("expected shared parent dir at %q, err=%v", sharedDir, err)
	}
}
