// Package gittest provides shell-out wrappers around the
// system `git` binary for test fixtures. Tests across the
// module use these to set up repos / branches / commits as
// preconditions, without importing go-git directly.
//
// Why a dedicated helper rather than each test reaching for
// exec.Command:
//
//   - Deterministic author / committer identity (test-stable
//     SHAs in regressions that pin commit hashes).
//   - LC_ALL=C so error messages don't get translated under
//     non-English CI environments.
//   - Single chokepoint for the "what's the test fixture
//     calling convention?" question — easier review and
//     consistent behavior.
//
// This package only exposes helpers for test setup. Production
// code talks to git via internal/fatclient/enjugit/internal/gitcli
// (through the enjugit Workflow API).
package gittest

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// defaultIdentity is the author/committer used by Commit and
// CommitAll when the caller doesn't specify one. Stable values
// keep test SHAs deterministic across runs.
const (
	defaultAuthorName  = "Test"
	defaultAuthorEmail = "test@example.com"
)

// Init creates an empty non-bare repo at dir with `main` as
// the default branch. Fatals on failure.
func Init(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("gittest.Init: mkdir %s: %v", dir, err)
	}
	mustRun(t, "", "init", "-b", "main", dir)
}

// InitBare creates an empty bare repo at dir with `main` as
// the default branch.
func InitBare(t testing.TB, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("gittest.InitBare: mkdir %s: %v", dir, err)
	}
	mustRun(t, "", "init", "--bare", "-b", "main", dir)
}

// Clone clones url into dir.
func Clone(t testing.TB, dir, url string) {
	t.Helper()
	mustRun(t, "", "clone", url, dir)
}

// CloneBranch clones url into dir and checks out the named
// branch.
func CloneBranch(t testing.TB, dir, url, branch string) {
	t.Helper()
	mustRun(t, "", "clone", "--branch", branch, url, dir)
}

// Run executes `git -C dir <args...>` and returns trimmed
// stdout. Fatals on non-zero exit. Use RunOK when you need
// to inspect failures non-fatally.
func Run(t testing.TB, dir string, args ...string) string {
	t.Helper()
	out, err := RunOK(t, dir, args...)
	if err != nil {
		t.Fatalf("gittest.Run: git %v in %s: %v\n%s", args, dir, err, out)
	}
	return out
}

// RunOK executes `git -C dir <args...>` and returns combined
// stdout/stderr without fataling. Used by helpers that want to
// classify failures (e.g. "does branch exist?") instead of
// aborting.
func RunOK(t testing.TB, dir string, args ...string) (string, error) {
	t.Helper()
	full := args
	if dir != "" {
		full = append([]string{"-C", dir}, args...)
	}
	cmd := exec.Command("git", full...)
	// Force C locale so error messages stay parseable / readable
	// under CI runs with non-default LANG.
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C")
	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	err := cmd.Run()
	return strings.TrimSpace(out.String()), err
}

// Commit writes content to path (relative to dir), stages it,
// and creates a commit with the default test identity. Returns
// the resulting commit SHA. Path may include subdirectories;
// they're created as needed.
func Commit(t testing.TB, dir, path, content, message string) string {
	t.Helper()
	return CommitAs(t, dir, path, content, message, defaultAuthorName, defaultAuthorEmail)
}

// CommitAs is Commit with a caller-supplied identity.
func CommitAs(t testing.TB, dir, path, content, message, name, email string) string {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("gittest.CommitAs: mkdir %s: %v", filepath.Dir(full), err)
	}
	if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
		t.Fatalf("gittest.CommitAs: write %s: %v", full, err)
	}
	mustRun(t, dir, "add", "--", path)
	mustRun(t, dir,
		"-c", "user.name="+name,
		"-c", "user.email="+email,
		"commit", "-m", message)
	return HeadSHA(t, dir)
}

// CommitAll stages every change in the worktree (tracked + new
// files matching `git add .`) and commits with the default
// identity. Returns the commit SHA.
func CommitAll(t testing.TB, dir, message string) string {
	t.Helper()
	mustRun(t, dir, "add", "-A")
	mustRun(t, dir,
		"-c", "user.name="+defaultAuthorName,
		"-c", "user.email="+defaultAuthorEmail,
		"commit", "-m", message)
	return HeadSHA(t, dir)
}

// Push pushes the named branch to origin. Fatals on failure.
func Push(t testing.TB, dir, branch string) {
	t.Helper()
	mustRun(t, dir, "push", "origin", branch)
}

// PushTo is Push with a named remote.
func PushTo(t testing.TB, dir, remote, branch string) {
	t.Helper()
	mustRun(t, dir, "push", remote, branch)
}

// AddRemote configures a remote on the repo at dir.
func AddRemote(t testing.TB, dir, name, url string) {
	t.Helper()
	mustRun(t, dir, "remote", "add", name, url)
}

// HeadSHA returns the SHA of HEAD in the repo at dir.
func HeadSHA(t testing.TB, dir string) string {
	t.Helper()
	return Run(t, dir, "rev-parse", "HEAD")
}

// RefSHA returns the SHA that ref resolves to. ref can be a
// short branch name ("main"), a full ref ("refs/heads/main"),
// or any rev-parse-acceptable expression.
func RefSHA(t testing.TB, dir, ref string) string {
	t.Helper()
	return Run(t, dir, "rev-parse", "--verify", ref)
}

// SetRef plants a ref pointing at sha.
func SetRef(t testing.TB, dir, ref, sha string) {
	t.Helper()
	mustRun(t, dir, "update-ref", ref, sha)
}

// CreateBranchAt creates a branch named `name` pointing at sha.
func CreateBranchAt(t testing.TB, dir, name, sha string) {
	t.Helper()
	mustRun(t, dir, "branch", name, sha)
}

// Checkout switches the worktree to the named branch (force).
func Checkout(t testing.TB, dir, branch string) {
	t.Helper()
	mustRun(t, dir, "checkout", "-f", branch)
}

// InitBareWithSeed creates a bare repo, plants one seed commit
// on main (a README.md), and pushes it. Returns the bare repo
// path. The seed dir is created under t.TempDir and is
// discarded by t.Cleanup — only the bare path lives on.
//
// Encapsulates the "bare + seed + push" pattern used as the
// fixture shape by ~half of the integration test files. Saves
// 20-ish lines of boilerplate per call site.
func InitBareWithSeed(t testing.TB, bare string) {
	t.Helper()
	InitBare(t, bare)
	seed := t.TempDir()
	Init(t, seed)
	AddRemote(t, seed, "origin", bare)
	Commit(t, seed, "README.md", "# seed\n", "seed")
	Push(t, seed, "main")
}

// mustRun is the lock-free chokepoint shared by every
// fatal-on-error helper.
func mustRun(t testing.TB, dir string, args ...string) {
	t.Helper()
	if _, err := RunOK(t, dir, args...); err != nil {
		out, _ := RunOK(t, dir, args...)
		t.Fatalf("gittest: git %v in %s: %v\n%s", args, dir, err, out)
	}
}
