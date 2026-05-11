package gitcli

import (
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// seedCommitOnMain creates a commit on main and returns its SHA.
// Test helper — uses real git CLI so we don't bootstrap from
// methods under test.
func seedCommitOnMain(t *testing.T, dir, filename, content string) string {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, filename), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", filename)
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "test: "+filename)
	out := gitRun(t, dir, "rev-parse", "HEAD")
	return strings.TrimSpace(out)
}

// --- ResolveRef ---

func TestResolveRefByBranchName(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	want := seedCommitOnMain(t, dir, "a.txt", "a")

	c, err := OpenClone(dir, "", nullLogger())
	if err != nil {
		t.Fatal(err)
	}
	got, err := c.ResolveRef("main")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != want {
		t.Errorf("ResolveRef = %s, want %s", got, want)
	}
}

func TestResolveRefByFullRefPath(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	want := seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.ResolveRef("refs/heads/main")
	if err != nil {
		t.Fatalf("ResolveRef: %v", err)
	}
	if got != want {
		t.Errorf("ResolveRef = %s, want %s", got, want)
	}
}

func TestResolveRefBy40HexSHA(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	want := seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.ResolveRef(want)
	if err != nil {
		t.Fatalf("ResolveRef on SHA: %v", err)
	}
	if got != want {
		t.Errorf("ResolveRef = %s, want %s", got, want)
	}
}

func TestResolveRefUnknownReturnsRefNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	if _, err := c.ResolveRef("no-such-branch"); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

func TestResolveRefUnknownSHAReturnsRefNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	if _, err := c.ResolveRef(bogus); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

// --- Head ---

func TestHeadOnBranch(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	want := seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	sha, branch, err := c.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if sha != want {
		t.Errorf("sha = %s, want %s", sha, want)
	}
	if branch != "main" {
		t.Errorf("branch = %q, want main", branch)
	}
}

func TestHeadDetached(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	want := seedCommitOnMain(t, dir, "a.txt", "a")
	gitRun(t, dir, "checkout", "--detach", want)

	c, _ := OpenClone(dir, "", nullLogger())
	sha, branch, err := c.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	if sha != want {
		t.Errorf("sha = %s, want %s", sha, want)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty (detached)", branch)
	}
}

func TestHeadUnbornReturnsRefNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir) // no commits → unborn HEAD

	c, _ := OpenClone(dir, "", nullLogger())
	if _, _, err := c.Head(); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound on unborn HEAD, got %v", err)
	}
}

// --- LocalBranches ---

func TestLocalBranches(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")
	gitRun(t, dir, "branch", "feature-x")
	gitRun(t, dir, "branch", "feature-y")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.LocalBranches()
	if err != nil {
		t.Fatalf("LocalBranches: %v", err)
	}
	sort.Strings(got)
	want := []string{"feature-x", "feature-y", "main"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestLocalBranchesEmpty(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir) // no commits yet → no local branches

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.LocalBranches()
	if err != nil {
		t.Fatalf("LocalBranches: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("expected empty, got %v", got)
	}
}

// --- LocalBranchHash ---

func TestLocalBranchHashLocalRef(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	want := seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.LocalBranchHash("main")
	if err != nil {
		t.Fatalf("LocalBranchHash: %v", err)
	}
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestLocalBranchHashFallsBackToOrigin(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	want := seedCommitOnMain(t, seed, "a.txt", "a")
	gitRun(t, seed, "push", "origin", "main")

	reader := filepath.Join(tmp, "reader")
	gitRun(t, ".", "clone", bare, reader)
	// Delete the local main ref to force the fallback path.
	gitRun(t, reader, "checkout", "--detach")
	gitRun(t, reader, "update-ref", "-d", "refs/heads/main")

	c, _ := OpenClone(reader, "", nullLogger())
	got, err := c.LocalBranchHash("main")
	if err != nil {
		t.Fatalf("LocalBranchHash: %v", err)
	}
	if got != want {
		t.Errorf("got %s, want %s (origin fallback)", got, want)
	}
}

func TestLocalBranchHashUnknownReturnsEmpty(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.LocalBranchHash("nope")
	if err != nil {
		t.Fatalf("LocalBranchHash: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

// --- RemoteBranchHash ---

func TestRemoteBranchHash(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	want := seedCommitOnMain(t, seed, "a.txt", "a")
	gitRun(t, seed, "push", "origin", "main")

	c, _ := OpenClone(seed, "", nullLogger())
	got, err := c.RemoteBranchHash("main")
	if err != nil {
		t.Fatalf("RemoteBranchHash: %v", err)
	}
	if got != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestRemoteBranchHashAbsentReturnsEmpty(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	seedCommitOnMain(t, seed, "a.txt", "a")
	gitRun(t, seed, "push", "origin", "main")

	c, _ := OpenClone(seed, "", nullLogger())
	got, err := c.RemoteBranchHash("never-pushed")
	if err != nil {
		t.Fatalf("RemoteBranchHash: %v", err)
	}
	if got != "" {
		t.Errorf("expected empty, got %s", got)
	}
}

// --- RemoteBranches ---

func TestRemoteBranches(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	seedCommitOnMain(t, seed, "a.txt", "a")
	gitRun(t, seed, "push", "origin", "main")
	gitRun(t, seed, "branch", "topic-a")
	gitRun(t, seed, "push", "origin", "topic-a")

	c, _ := OpenClone(seed, "", nullLogger())
	got, err := c.RemoteBranches()
	if err != nil {
		t.Fatalf("RemoteBranches: %v", err)
	}
	sort.Strings(got)
	want := []string{"main", "topic-a"}
	if !equalSlice(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

// --- IsAncestor ---

func TestIsAncestorTrue(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	a := seedCommitOnMain(t, dir, "a.txt", "a")
	b := seedCommitOnMain(t, dir, "b.txt", "b")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.IsAncestor(a, b)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !got {
		t.Errorf("IsAncestor(%s, %s) = false, want true", a, b)
	}
}

func TestIsAncestorFalse(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	a := seedCommitOnMain(t, dir, "a.txt", "a")
	b := seedCommitOnMain(t, dir, "b.txt", "b")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.IsAncestor(b, a) // reversed
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if got {
		t.Errorf("IsAncestor(%s, %s) = true, want false", b, a)
	}
}

func TestIsAncestorSelfReturnsTrue(t *testing.T) {
	// Native git contract: a commit IS an ancestor of itself
	// (--is-ancestor returns 0). Pin so callers know.
	dir := t.TempDir()
	gitInit(t, dir)
	a := seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	got, err := c.IsAncestor(a, a)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if !got {
		t.Errorf("IsAncestor(a, a) = false, want true (reflexive)")
	}
}

func TestIsAncestorUnknownCommitReturnsFalse(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	a := seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	got, err := c.IsAncestor(bogus, a)
	if err != nil {
		t.Fatalf("IsAncestor: %v", err)
	}
	if got {
		t.Errorf("IsAncestor with unknown SHA = true, want false (matches gitv6 contract)")
	}
}

// --- State ---

func TestStateClean(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	if got := c.State(); got != StateClean {
		t.Errorf("State = %s, want clean", got)
	}
}

func TestStateDirtyTracked(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if got := c.State(); got != StateDirtyTracked {
		t.Errorf("State = %s, want dirty-tracked", got)
	}
}

func TestStateDirtyUntracked(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")
	if err := os.WriteFile(filepath.Join(dir, "untracked.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if got := c.State(); got != StateDirtyUntracked {
		t.Errorf("State = %s, want dirty-untracked", got)
	}
}

func TestStateDetached(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	a := seedCommitOnMain(t, dir, "a.txt", "a")
	gitRun(t, dir, "checkout", "--detach", a)

	c, _ := OpenClone(dir, "", nullLogger())
	if got := c.State(); got != StateDetached {
		t.Errorf("State = %s, want detached", got)
	}
}

// --- HeadCommitTime ---

func TestHeadCommitTimeMatchesCommit(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "a")

	c, _ := OpenClone(dir, "", nullLogger())
	tm := c.HeadCommitTime()
	if tm.IsZero() {
		t.Error("HeadCommitTime returned zero on valid HEAD")
	}
}

func TestHeadCommitTimeEmptyRepoZero(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)

	c, _ := OpenClone(dir, "", nullLogger())
	if !c.HeadCommitTime().IsZero() {
		t.Error("HeadCommitTime should be zero on unborn HEAD")
	}
}

// --- helpers ---

func equalSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
