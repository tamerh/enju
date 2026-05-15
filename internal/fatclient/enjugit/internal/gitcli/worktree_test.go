package gitcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- Checkout (force) ---

func TestCheckoutSwitchesBranch(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	gitRun(t, dir, "branch", "feature-x")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.Checkout("feature-x"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	_, branch, _ := c.Head()
	if branch != "feature-x" {
		t.Errorf("on %q, want feature-x", branch)
	}
}

func TestCheckoutForceOverwritesDirtyTracked(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "original")
	gitRun(t, dir, "branch", "feature-x")
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.Checkout("feature-x"); err != nil {
		t.Fatalf("force Checkout should succeed even on dirty: %v", err)
	}
	// File should now be back to "original" (force discards
	// tracked-file modifications).
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "original" {
		t.Errorf("a.txt = %q, want original (force overwrote)", got)
	}
}

func TestCheckoutPreservesUntracked(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	gitRun(t, dir, "branch", "feature-x")
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.Checkout("feature-x"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.txt")); err != nil {
		t.Errorf("untracked file lost across force checkout: %v", err)
	}
}

func TestCheckoutErrRefNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	err := c.Checkout("nope")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

// --- CheckoutCommit ---

func TestCheckoutCommitDetaches(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CheckoutCommit(s1); err != nil {
		t.Fatalf("CheckoutCommit: %v", err)
	}
	sha, branch, _ := c.Head()
	if sha != s1 {
		t.Errorf("HEAD = %s, want %s", sha, s1)
	}
	if branch != "" {
		t.Errorf("branch = %q, want empty (detached)", branch)
	}
}

func TestCheckoutCommitErrCommitNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	err := c.CheckoutCommit(bogus)
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

// --- CheckoutBranch (soft) ---

func TestCheckoutBranchEmptyIsNoop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CheckoutBranch(""); err != nil {
		t.Errorf("empty branch should no-op, got %v", err)
	}
}

func TestCheckoutBranchSameBranchNoop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	// Dirty tracked file: the no-op path must succeed regardless.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CheckoutBranch("main"); err != nil {
		t.Errorf("same-branch fast path failed: %v", err)
	}
	// Dirty content should still be there — same-branch path
	// must not trigger any worktree write.
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "dirty" {
		t.Errorf("worktree mutated on no-op: %q", got)
	}
}

func TestCheckoutBranchRefusesOnDirtyByNooping(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "original")
	gitRun(t, dir, "branch", "feature-x")
	// Make tree dirty AFTER branch is created so checkout would
	// be a real switch.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CheckoutBranch("feature-x"); err != nil {
		t.Errorf("dirty-tree CheckoutBranch should silently no-op, got %v", err)
	}
	// Still on main (no-op).
	_, branch, _ := c.Head()
	if branch != "main" {
		t.Errorf("branch = %q, want main (no-op on dirty)", branch)
	}
	// Dirty content preserved.
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "dirty" {
		t.Errorf("dirty content lost: %q", got)
	}
}

func TestCheckoutBranchAutoPlantFromOrigin(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	seedCommitOnMain(t, seed, "a.txt", "x")
	gitRun(t, seed, "push", "origin", "main")
	gitRun(t, seed, "branch", "feature-x")
	gitRun(t, seed, "push", "origin", "feature-x")

	// Reader: clone, but feature-x has no local ref (just
	// origin/feature-x).
	reader := filepath.Join(tmp, "reader")
	gitRun(t, ".", "clone", bare, reader)
	if _, err := os.Stat(filepath.Join(reader, ".git", "refs", "heads", "feature-x")); err == nil {
		// Some git versions auto-create local refs at clone time;
		// remove for the test.
		gitRun(t, reader, "update-ref", "-d", "refs/heads/feature-x")
	}

	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.CheckoutBranch("feature-x"); err != nil {
		t.Fatalf("auto-plant failed: %v", err)
	}
	_, branch, _ := c.Head()
	if branch != "feature-x" {
		t.Errorf("on %q, want feature-x", branch)
	}
	// Local ref should now exist.
	if sha, _ := c.LocalBranchHash("feature-x"); sha == "" {
		t.Errorf("local ref refs/heads/feature-x not planted")
	}
}

func TestCheckoutBranchErrRefNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	err := c.CheckoutBranch("never-existed")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

// --- ResetClean ---

func TestResetCleanReturnsToHEADAndRemovesUntracked(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	// Modify tracked file.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add untracked file.
	if err := os.WriteFile(filepath.Join(dir, "scratch.txt"), []byte("u"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Add untracked dir.
	if err := os.MkdirAll(filepath.Join(dir, "scratchdir"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "scratchdir", "inner.txt"), []byte("i"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.ResetClean(); err != nil {
		t.Fatalf("ResetClean: %v", err)
	}
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "x" {
		t.Errorf("tracked file not restored: %q", got)
	}
	if _, err := os.Stat(filepath.Join(dir, "scratch.txt")); !os.IsNotExist(err) {
		t.Error("untracked file not removed")
	}
	if _, err := os.Stat(filepath.Join(dir, "scratchdir")); !os.IsNotExist(err) {
		t.Error("untracked dir not removed")
	}
	if c.State() != StateClean {
		t.Errorf("state = %s, want clean", c.State())
	}
}

func TestResetCleanPreservesBareGitAndClone(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	if err := os.MkdirAll(filepath.Join(dir, ".bare.git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".bare.git", "marker"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".clone"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".clone", "marker"), []byte("c"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.ResetClean(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".bare.git", "marker")); err != nil {
		t.Errorf(".bare.git was nuked: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".clone", "marker")); err != nil {
		t.Errorf(".clone was nuked: %v", err)
	}
}

// --- SyncIndexToHead ---

func TestSyncIndexToHead(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	// Stage a deletion-modification so the index disagrees with
	// HEAD.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("modified"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt")
	// Index now has "modified" content; worktree also has it;
	// HEAD has "x". SyncIndexToHead should reset index → "x"
	// but leave worktree at "modified".

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.SyncIndexToHead(); err != nil {
		t.Fatalf("SyncIndexToHead: %v", err)
	}
	// Worktree content unchanged.
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "modified" {
		t.Errorf("worktree changed: %q", got)
	}
	// Index now matches HEAD: `git diff --cached` should be empty.
	out := gitRun(t, dir, "diff", "--cached")
	if strings.TrimSpace(out) != "" {
		t.Errorf("index doesn't match HEAD: %s", out)
	}
}

// --- FastForwardWorktree ---

func TestFastForwardWorktree(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "old")
	oldSHA := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	// Build the "new" commit: modify a.txt, add b.txt.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("added"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "a.txt", "b.txt")
	gitRun(t, dir, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-m", "new")
	newSHA := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))

	// Reproduce the post-ref-only-FF precondition: the branch ref
	// is at newSHA, but index + worktree still reflect oldSHA.
	gitRun(t, dir, "read-tree", "--reset", "-u", oldSHA)
	// An untracked file that MUST survive (the whole reason the
	// FF path historically skipped Checkout).
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.FastForwardWorktree(oldSHA, newSHA); err != nil {
		t.Fatalf("FastForwardWorktree: %v", err)
	}

	// a.txt updated, b.txt materialized on disk.
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "new" {
		t.Errorf("a.txt = %q, want %q", got, "new")
	}
	if got, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil || string(got) != "added" {
		t.Errorf("b.txt not materialized: %q err=%v", got, err)
	}
	// Untracked file preserved.
	if got, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(got) != "untracked" {
		t.Errorf("untracked keep.txt lost: %q err=%v", got, err)
	}
	// Index now matches newSHA → clean status (no phantom delete).
	if out := strings.TrimSpace(gitRun(t, dir, "status", "--porcelain", "--untracked-files=no")); out != "" {
		t.Errorf("worktree/index not clean vs HEAD: %s", out)
	}

	// Unresolvable SHA → typed ErrRefNotFound.
	if err := c.FastForwardWorktree("deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", newSHA); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("bad oldSHA: got %v, want ErrRefNotFound", err)
	}
}

// --- ReconcileWorktreeToHead ---

func TestReconcileWorktreeToHead(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "v1")
	// Add a second tracked file in a follow-up commit so HEAD has
	// {a.txt, b.txt}.
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("btext"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "b.txt")
	gitRun(t, dir, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-m", "add b")

	// Simulate a stale worktree: tracked b.txt missing from disk,
	// a.txt clobbered with wrong content — but the ref/HEAD is
	// correct. (This is the post-partial-FF / empty-delta shape
	// the FF path can't fix via read-tree.)
	if err := os.Remove(filepath.Join(dir, "b.txt")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("STALE"), 0o644); err != nil {
		t.Fatal(err)
	}
	// Untracked file that MUST survive.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.ReconcileWorktreeToHead(); err != nil {
		t.Fatalf("ReconcileWorktreeToHead: %v", err)
	}

	// Missing tracked file materialized; clobbered one restored.
	if got, err := os.ReadFile(filepath.Join(dir, "b.txt")); err != nil || string(got) != "btext" {
		t.Errorf("b.txt not materialized: %q err=%v", got, err)
	}
	if got, _ := os.ReadFile(filepath.Join(dir, "a.txt")); string(got) != "v1" {
		t.Errorf("a.txt = %q, want %q", got, "v1")
	}
	// Untracked preserved.
	if got, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(got) != "untracked" {
		t.Errorf("untracked keep.txt lost: %q err=%v", got, err)
	}
	// Clean vs HEAD (no phantom delete).
	if out := strings.TrimSpace(gitRun(t, dir, "status", "--porcelain", "--untracked-files=no")); out != "" {
		t.Errorf("worktree/index not clean vs HEAD: %s", out)
	}
}

// TestReconcileWorktreeToHead_CollidingUntrackedOverwritten pins
// the intentional asymmetry with FastForwardWorktree: where an
// untracked file shares a path with a tracked HEAD entry, the
// committed bytes win (no error). This is the actual
// collision-retry shape the targetSHA==NewTip branch exists to
// recover, and the only case where the untracked behavior is
// subtle — FastForwardWorktree REFUSES it, Reconcile overrides.
func TestReconcileWorktreeToHead_CollidingUntrackedOverwritten(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "base")
	// HEAD commit adds tracked results.csv (the merged-in artifact).
	if err := os.WriteFile(filepath.Join(dir, "results.csv"), []byte("COMMITTED"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "add", "results.csv")
	gitRun(t, dir, "-c", "user.email=t@e", "-c", "user.name=t", "commit", "-m", "add results")
	headSHA := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	baseSHA := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD~1"))

	// Reproduce the collision-retry shape: ref/HEAD = headSHA, but
	// index is back at base (no results.csv) and disk has an
	// UNTRACKED results.csv with stale scratch content at the same
	// path the HEAD commit tracks.
	gitRun(t, dir, "read-tree", "--reset", "-u", baseSHA)
	_ = headSHA // ref/branch still points here; only index/worktree moved
	if err := os.WriteFile(filepath.Join(dir, "results.csv"), []byte("scratch-leftover"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A non-colliding untracked file must still survive.
	if err := os.WriteFile(filepath.Join(dir, "keep.txt"), []byte("untracked"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.ReconcileWorktreeToHead(); err != nil {
		t.Fatalf("ReconcileWorktreeToHead must recover (not error) on collision: %v", err)
	}

	// Committed bytes win at the colliding path.
	if got, _ := os.ReadFile(filepath.Join(dir, "results.csv")); string(got) != "COMMITTED" {
		t.Errorf("colliding path: got %q, want committed %q", got, "COMMITTED")
	}
	// Non-colliding untracked file untouched.
	if got, err := os.ReadFile(filepath.Join(dir, "keep.txt")); err != nil || string(got) != "untracked" {
		t.Errorf("non-colliding untracked lost: %q err=%v", got, err)
	}
	// Clean vs HEAD.
	if out := strings.TrimSpace(gitRun(t, dir, "status", "--porcelain", "--untracked-files=no")); out != "" {
		t.Errorf("not clean vs HEAD: %s", out)
	}
}

// --- RemoveFiles ---

func TestRemoveFiles(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.txt"), []byte("b"), 0o644); err != nil {
		t.Fatal(err)
	}

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.RemoveFiles([]string{"a.txt", "no-such.txt", "b.txt"}); err != nil {
		t.Fatalf("RemoveFiles: %v", err)
	}
	for _, p := range []string{"a.txt", "b.txt"} {
		if _, err := os.Stat(filepath.Join(dir, p)); !os.IsNotExist(err) {
			t.Errorf("%s not removed", p)
		}
	}
}

// --- CheckoutBranchFrom ---

func TestCheckoutBranchFromCreatesNewFromBase(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	// main has 2 commits, runBranch forks at first.
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	commitWithMessage(t, dir, "b.txt", "2", "second")
	// runBranch points at s1 (an "older" commit).
	gitRun(t, dir, "branch", "runBranch", s1)

	c, _ := OpenClone(dir, "", nullLogger())
	// Create topic-x forking from runBranch (s1), NOT main (s2).
	if err := c.CheckoutBranchFrom("topic-x", "runBranch", "main"); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}
	sha, branch, _ := c.Head()
	if branch != "topic-x" {
		t.Errorf("on %q, want topic-x", branch)
	}
	if sha != s1 {
		t.Errorf("topic-x tip = %s, want %s (runBranch)", sha, s1)
	}
}

func TestCheckoutBranchFromExistingBranchAncestorOk(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	s1 := commitWithMessage(t, dir, "a.txt", "1", "first")
	gitRun(t, dir, "branch", "runBranch", s1)
	gitRun(t, dir, "branch", "topic-x", s1) // pre-existing topic forked from runBranch

	c, _ := OpenClone(dir, "", nullLogger())
	// runBranch's tip (s1) IS an ancestor of topic-x's tip (s1).
	// Existing ref is fine — should just checkout.
	if err := c.CheckoutBranchFrom("topic-x", "runBranch", "main"); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}
	_, branch, _ := c.Head()
	if branch != "topic-x" {
		t.Errorf("on %q, want topic-x", branch)
	}
}

func TestCheckoutBranchFromStaleRefResets(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	rootSHA := commitWithMessage(t, dir, "seed.txt", "0", "seed")
	runTipSHA := commitWithMessage(t, dir, "run.txt", "1", "run commit")
	// runBranch tip = runTipSHA.
	gitRun(t, dir, "branch", "runBranch", runTipSHA)
	// STALE topic ref: planted at rootSHA which is BEFORE
	// runBranch's tip. runBranch tip is NOT an ancestor of
	// topic-x's tip → stale.
	gitRun(t, dir, "branch", "topic-x", rootSHA)

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CheckoutBranchFrom("topic-x", "runBranch", "main"); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}
	// topic-x should now be at runTipSHA (recreated from
	// runBranch).
	sha, _ := c.LocalBranchHash("topic-x")
	if sha != runTipSHA {
		t.Errorf("topic-x tip = %s, want %s (recreated from runBranch)", sha, runTipSHA)
	}
}

func TestCheckoutBranchFromFallsBackToDefault(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	mainSHA := commitWithMessage(t, dir, "a.txt", "x", "first")
	// No baseBranch given, no origin, no pre-existing topic-x.
	// Should fall back to defaultBranch (main).

	c, _ := OpenClone(dir, "", nullLogger())
	if err := c.CheckoutBranchFrom("topic-x", "", "main"); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}
	sha, _ := c.LocalBranchHash("topic-x")
	if sha != mainSHA {
		t.Errorf("topic-x tip = %s, want %s (default base)", sha, mainSHA)
	}
}

func TestCheckoutBranchFromFollowsUpstreamWhenNoBase(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	seedCommitOnMain(t, seed, "a.txt", "x")
	gitRun(t, seed, "push", "origin", "main")
	topicSHA := commitWithMessage(t, seed, "b.txt", "y", "topic")
	gitRun(t, seed, "branch", "topic-x")
	gitRun(t, seed, "push", "origin", "topic-x")

	// Reader: clone, has origin/topic-x but no local topic-x.
	reader := filepath.Join(tmp, "reader")
	gitRun(t, ".", "clone", bare, reader)
	// Remove any auto-created local topic-x.
	_, _ = runGit(reader, []string{"update-ref", "-d", "refs/heads/topic-x"}, runOpts{})

	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.CheckoutBranchFrom("topic-x", "", "main"); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}
	sha, _ := c.LocalBranchHash("topic-x")
	if sha != topicSHA {
		t.Errorf("topic-x tip = %s, want %s (origin tip)", sha, topicSHA)
	}
}

func TestCheckoutBranchFromSameBranchNoop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")
	gitRun(t, dir, "branch", "feature-x")
	gitRun(t, dir, "checkout", "feature-x")

	c, _ := OpenClone(dir, "", nullLogger())
	// Already on feature-x.
	if err := c.CheckoutBranchFrom("feature-x", "main", "main"); err != nil {
		t.Errorf("same-branch should no-op, got %v", err)
	}
	_, branch, _ := c.Head()
	if branch != "feature-x" {
		t.Errorf("branch = %q, want feature-x", branch)
	}
}

func TestCheckoutBranchFromUpstreamRefDisagreesResets(t *testing.T) {
	tmp := t.TempDir()
	bare := filepath.Join(tmp, "bare.git")
	gitInitBare(t, bare)
	seed := filepath.Join(tmp, "seed")
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	commitWithMessage(t, seed, "a.txt", "x", "first")
	gitRun(t, seed, "push", "origin", "main")
	topicSHA := commitWithMessage(t, seed, "b.txt", "y", "topic")
	gitRun(t, seed, "branch", "topic-x")
	gitRun(t, seed, "push", "origin", "topic-x")

	// Reader clone, then plant a stale local topic-x at a
	// different SHA than origin's.
	reader := filepath.Join(tmp, "reader")
	gitRun(t, ".", "clone", bare, reader)
	mainSHA := strings.TrimSpace(gitRun(t, reader, "rev-parse", "refs/heads/main"))
	_, _ = runGit(reader, []string{"update-ref", "-d", "refs/heads/topic-x"}, runOpts{})
	gitRun(t, reader, "update-ref", "refs/heads/topic-x", mainSHA)
	// Now local topic-x = mainSHA; origin/topic-x = topicSHA.

	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.CheckoutBranchFrom("topic-x", "", "main"); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}
	sha, _ := c.LocalBranchHash("topic-x")
	if sha != topicSHA {
		t.Errorf("topic-x tip = %s, want %s (origin overrode stale local)", sha, topicSHA)
	}
}
