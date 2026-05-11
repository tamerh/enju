package gitcli

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// nullLogger discards everything. Tests don't need slog output.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// gitInit creates an empty non-bare repo at dir via the real git
// CLI. Used by tests as a fixture so the gitcli package isn't
// bootstrapping itself.
func gitInit(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "-b", "main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init %s: %v\n%s", dir, err, out)
	}
}

// gitInitBare creates an empty bare repo at dir.
func gitInitBare(t *testing.T, dir string) {
	t.Helper()
	cmd := exec.Command("git", "init", "--bare", "-b", "main", dir)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git init --bare %s: %v\n%s", dir, err, out)
	}
}

// gitRun runs `git -C dir <args...>` and fatals on error. Used by
// tests to set up state without going through Clone methods that
// aren't ported yet.
func gitRun(t *testing.T, dir string, args ...string) string {
	t.Helper()
	full := append([]string{"-C", dir}, args...)
	cmd := exec.Command("git", full...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return string(out)
}

// TestStaticCloneSatisfiesOps pins the interface contract — if
// Clone ever drops a method from Ops (or Ops grows a method
// Clone doesn't implement), this fails at compile time. The
// runtime check is redundant with the var declaration but makes
// the intent explicit.
func TestStaticCloneSatisfiesOps(t *testing.T) {
	var _ Ops = (*Clone)(nil)
}

// TestRunGitBasic pins the runGit helper's success path: a real
// `git version` invocation should return non-empty stdout.
func TestRunGitBasic(t *testing.T) {
	out, err := runGit("", []string{"version"}, runOpts{})
	if err != nil {
		t.Fatalf("git version: %v", err)
	}
	if !strings.HasPrefix(string(out), "git version") {
		t.Errorf("expected stdout to start with 'git version', got %q", out)
	}
}

// TestRunGitClassifiesMissingCommit verifies the stderr → typed-
// error mapping fires for the rev-parse-on-bogus-SHA case.
func TestRunGitClassifiesMissingCommit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	// Use `git show` (which actually fails) rather than rev-parse,
	// which just echoes a 40-char hex string back without
	// validation. Lesson encoded here so Phase 1 verbs use
	// rev-parse --verify or cat-file -e when existence matters.
	_, err := runGit(dir, []string{"show", "--no-patch", "deadbeef0123456789abcdef0123456789abcdef"}, runOpts{})
	if err == nil {
		t.Fatal("expected error for nonexistent SHA")
	}
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

// TestOpenCloneMissingDirReturnsErrCloneNotFound pins the
// "explicit not-found" contract: callers up the stack rely on
// errors.Is(ErrCloneNotFound) to distinguish "no clone here" from
// other failures.
func TestOpenCloneMissingDirReturnsErrCloneNotFound(t *testing.T) {
	dir := t.TempDir() // exists but has no .git
	_, err := OpenClone(dir, "", nullLogger())
	if err == nil {
		t.Fatal("expected ErrCloneNotFound")
	}
	if !errors.Is(err, ErrCloneNotFound) {
		t.Errorf("expected ErrCloneNotFound, got %v", err)
	}
}

// TestOpenCloneHydratesRemoteURL pins the origin-discovery
// behavior at open time. Sets up a repo with an origin, opens it
// via gitcli, expects RemoteURL() to return what was configured.
func TestOpenCloneHydratesRemoteURL(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	gitRun(t, dir, "remote", "add", "origin", "/some/remote/path")

	c, err := OpenClone(dir, "", nullLogger())
	if err != nil {
		t.Fatalf("OpenClone: %v", err)
	}
	if c.RemoteURL() != "/some/remote/path" {
		t.Errorf("RemoteURL = %q, want /some/remote/path", c.RemoteURL())
	}
	if c.WorkDir() != dir {
		t.Errorf("WorkDir = %q, want %q", c.WorkDir(), dir)
	}
}

// TestOpenCloneNoOriginIsFine pins the path-mode bootstrap case:
// a clone without origin opens cleanly, RemoteURL() returns "".
func TestOpenCloneNoOriginIsFine(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	c, err := OpenClone(dir, "", nullLogger())
	if err != nil {
		t.Fatalf("OpenClone: %v", err)
	}
	if c.RemoteURL() != "" {
		t.Errorf("RemoteURL = %q, want empty", c.RemoteURL())
	}
}

// TestCloneOrInitClonesFromBare pins the clone path: bare repo
// with a seed commit, CloneOrInit produces a working clone.
func TestCloneOrInitClonesFromBare(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)

	// Seed the bare via a throwaway worktree.
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	if err := os.WriteFile(filepath.Join(seed, "README.md"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, seed, "-c", "user.name=t", "-c", "user.email=t@x", "add", "README.md")
	gitRun(t, seed, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "seed")
	gitRun(t, seed, "push", "origin", "main")

	// Clone via gitcli.
	target := filepath.Join(tmp, "clone")
	c, err := CloneOrInit(target, bare, "", nullLogger())
	if err != nil {
		t.Fatalf("CloneOrInit: %v", err)
	}
	if c.WorkDir() != target {
		t.Errorf("WorkDir = %q, want %q", c.WorkDir(), target)
	}
	if c.RemoteURL() != bare {
		t.Errorf("RemoteURL = %q, want %q", c.RemoteURL(), bare)
	}
	if _, err := os.Stat(filepath.Join(target, "README.md")); err != nil {
		t.Errorf("README.md not in fresh clone: %v", err)
	}
}

// TestInitLocalCreatesEmptyMain pins the path-mode bootstrap:
// InitLocal creates an empty non-bare repo with main as the
// default branch.
func TestInitLocalCreatesEmptyMain(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "fresh")
	c, err := InitLocal(dir, "", nullLogger())
	if err != nil {
		t.Fatalf("InitLocal: %v", err)
	}
	if c.WorkDir() != dir {
		t.Errorf("WorkDir = %q, want %q", c.WorkDir(), dir)
	}
	// HEAD should point at refs/heads/main even though no commit
	// exists yet — symbolic-ref reads the unborn ref cleanly.
	out := gitRun(t, dir, "symbolic-ref", "HEAD")
	if !strings.Contains(out, "refs/heads/main") {
		t.Errorf("HEAD symbolic-ref = %q, want refs/heads/main", out)
	}
}

