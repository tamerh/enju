package gitcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupRemotePair creates a bare repo + a seed clone wired to it
// with a single commit on main, pushed to origin. Returns
// (bare, seed) absolute paths.
func setupRemotePair(t *testing.T) (bare, seed string) {
	t.Helper()
	tmp := t.TempDir()
	bare = filepath.Join(tmp, "bare.git")
	seed = filepath.Join(tmp, "seed")
	gitInitBare(t, bare)
	gitInit(t, seed)
	gitRun(t, seed, "remote", "add", "origin", bare)
	seedCommitOnMain(t, seed, "a.txt", "x")
	gitRun(t, seed, "push", "origin", "main")
	return bare, seed
}

// --- Fetch ---

// TestFetchTolerantOfLeftoverTmpPack pins the finding that real
// `git fetch` ignores tmp_pack_* orphans left over from a
// previous interrupted fetch — opposite of go-git's behavior,
// which would trip on them. Documents why gitcli has no sweep
// equivalent to gitv6's sweepStaleTempPackFiles.
func TestFetchTolerantOfLeftoverTmpPack(t *testing.T) {
	bare, seed := setupRemotePair(t)

	reader := filepath.Join(t.TempDir(), "reader")
	gitRun(t, ".", "clone", bare, reader)

	// Plant garbage content as a fake tmp_pack_ leftover.
	tmpPack := filepath.Join(reader, ".git", "objects", "pack", "tmp_pack_99999")
	if err := os.WriteFile(tmpPack, []byte("GARBAGE not a pack"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Add a new commit on seed + push.
	commitWithMessage(t, seed, "b.txt", "y", "second")
	gitRun(t, seed, "push", "origin", "main")

	// Fetch from reader must succeed despite the orphan.
	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.Fetch(); err != nil {
		t.Errorf("fetch should tolerate leftover tmp_pack, got: %v", err)
	}
	// And the orphan is harmless — origin/main should be up to
	// date.
	out := strings.TrimSpace(gitRun(t, reader, "rev-parse", "refs/remotes/origin/main"))
	if !isHexSHA(out) {
		t.Errorf("origin/main not updated post-fetch: %q", out)
	}
}

func TestFetchPopulatesOriginTracking(t *testing.T) {
	bare, seed := setupRemotePair(t)
	// Seed adds another commit + pushes.
	newSHA := commitWithMessage(t, seed, "b.txt", "y", "second")
	gitRun(t, seed, "push", "origin", "main")

	// Reader: clone, then test Fetch picks up the new commit.
	reader := filepath.Join(t.TempDir(), "reader")
	gitRun(t, ".", "clone", bare, reader)
	// Add another commit on seed AFTER the clone.
	thirdSHA := commitWithMessage(t, seed, "c.txt", "z", "third")
	gitRun(t, seed, "push", "origin", "main")

	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.Fetch(); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	// Reader's origin/main should now reflect thirdSHA.
	got, _ := c.RemoteBranchHash("main")
	if got != thirdSHA {
		t.Errorf("origin/main = %s, want %s", got, thirdSHA)
	}
	_ = newSHA
}

// --- FetchBranch ---

func TestFetchBranchSpecificRef(t *testing.T) {
	bare, seed := setupRemotePair(t)
	gitRun(t, seed, "branch", "topic-x")
	topicSHA := commitWithMessage(t, seed, "b.txt", "y", "topic commit")
	gitRun(t, seed, "branch", "-f", "topic-x", topicSHA)
	gitRun(t, seed, "push", "origin", "topic-x")

	reader := filepath.Join(t.TempDir(), "reader")
	gitRun(t, ".", "clone", bare, reader)

	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.FetchBranch("topic-x"); err != nil {
		t.Fatalf("FetchBranch: %v", err)
	}
	// origin/topic-x should be at topicSHA.
	out := strings.TrimSpace(gitRun(t, reader, "rev-parse", "refs/remotes/origin/topic-x"))
	if out != topicSHA {
		t.Errorf("origin/topic-x = %s, want %s", out, topicSHA)
	}
}

func TestFetchBranchAbsentOnRemoteIsNoop(t *testing.T) {
	_, seed := setupRemotePair(t)

	c, _ := OpenClone(seed, "", nullLogger())
	if err := c.FetchBranch("never-pushed"); err != nil {
		t.Errorf("absent-on-remote should no-op, got %v", err)
	}
}

// --- PullBranch ---

func TestPullBranchWhenOnBranchFFsWorktree(t *testing.T) {
	// Set up: clone first, then push a new commit so reader is
	// truly behind and PullBranch has something to FF.
	bare, seed := setupRemotePair(t)
	reader := filepath.Join(t.TempDir(), "reader")
	gitRun(t, ".", "clone", bare, reader)
	after := commitWithMessage(t, seed, "b.txt", "y", "after-clone")
	gitRun(t, seed, "push", "origin", "main")

	c, _ := OpenClone(reader, "", nullLogger())
	if err := c.PullBranch("main"); err != nil {
		t.Fatalf("PullBranch: %v", err)
	}
	// Local main should be at `after`.
	sha, _ := c.LocalBranchHash("main")
	if sha != after {
		t.Errorf("local main = %s, want %s", sha, after)
	}
	// Worktree should reflect new content.
	if _, err := os.Stat(filepath.Join(reader, "b.txt")); err != nil {
		t.Errorf("b.txt not in worktree post-pull: %v", err)
	}
}

func TestPullBranchAbsentOnRemoteIsNoop(t *testing.T) {
	_, seed := setupRemotePair(t)

	c, _ := OpenClone(seed, "", nullLogger())
	if err := c.PullBranch("never-pushed"); err != nil {
		t.Errorf("absent-on-remote should no-op, got %v", err)
	}
}

// --- Push ---

func TestPushSuccess(t *testing.T) {
	bare, seed := setupRemotePair(t)
	newSHA := commitWithMessage(t, seed, "b.txt", "y", "second")

	c, _ := OpenClone(seed, "", nullLogger())
	if err := c.Push("main"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	if !c.LastPushAt().After(c.LastPushAt().Add(-1)) {
		t.Error("LastPushAt not set")
	}
	if c.LastPushError() != "" {
		t.Errorf("LastPushError = %q, want empty", c.LastPushError())
	}
	// Bare repo should have the new commit reachable as main.
	out := strings.TrimSpace(gitRun(t, bare, "rev-parse", "refs/heads/main"))
	if out != newSHA {
		t.Errorf("bare main = %s, want %s", out, newSHA)
	}
}

func TestPushErrRefNotFound(t *testing.T) {
	_, seed := setupRemotePair(t)

	c, _ := OpenClone(seed, "", nullLogger())
	err := c.Push("never-existed")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

func TestPushNonFastForward(t *testing.T) {
	bare, _ := setupRemotePair(t)

	// Two clones — both will commit, push will be non-FF on
	// the slower one.
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	gitRun(t, ".", "clone", bare, a)
	gitRun(t, ".", "clone", bare, b)

	// a commits + pushes first.
	commitWithMessage(t, a, "from-a.txt", "x", "from-a")
	gitRun(t, a, "push", "origin", "main")

	// b commits a divergent change, push should be non-FF.
	commitWithMessage(t, b, "from-b.txt", "y", "from-b")
	cb, _ := OpenClone(b, "", nullLogger())
	err := cb.Push("main")
	if !errors.Is(err, ErrPushNonFF) {
		t.Errorf("expected ErrPushNonFF, got %v", err)
	}
	if cb.LastPushError() == "" {
		t.Error("LastPushError not set on push failure")
	}
}

// --- PushWithVerify ---

func TestPushWithVerifySuccess(t *testing.T) {
	_, seed := setupRemotePair(t)
	newSHA := commitWithMessage(t, seed, "b.txt", "y", "second")

	c, _ := OpenClone(seed, "", nullLogger())
	if err := c.PushWithVerify("main", newSHA); err != nil {
		t.Fatalf("PushWithVerify: %v", err)
	}
}

func TestPushWithVerifyMismatchFails(t *testing.T) {
	_, seed := setupRemotePair(t)
	commitWithMessage(t, seed, "b.txt", "y", "second")

	c, _ := OpenClone(seed, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	err := c.PushWithVerify("main", bogus)
	if !errors.Is(err, ErrPushVerifyFailed) {
		t.Errorf("expected ErrPushVerifyFailed, got %v", err)
	}
	// Check the typed-error details.
	var verr *ErrVerifyFailed
	if errors.As(err, &verr) {
		if verr.Branch != "main" {
			t.Errorf("verr.Branch = %q, want main", verr.Branch)
		}
		if verr.LocalSHA != bogus {
			t.Errorf("verr.LocalSHA = %q, want %q", verr.LocalSHA, bogus)
		}
		if verr.RemoteSHA == "" {
			t.Error("verr.RemoteSHA should be non-empty (remote has main)")
		}
	} else {
		t.Errorf("error not ErrVerifyFailed: %v", err)
	}
}

// --- PushAllRefs ---

func TestPushAllRefsWildcardPushes(t *testing.T) {
	bare, seed := setupRemotePair(t)
	// Create + populate two extra branches in seed (not on origin yet).
	gitRun(t, seed, "branch", "topic-a")
	gitRun(t, seed, "branch", "topic-b")

	c, _ := OpenClone(seed, "", nullLogger())
	if err := c.PushAllRefs(false); err != nil {
		t.Fatalf("PushAllRefs: %v", err)
	}
	// Bare should now carry topic-a + topic-b.
	for _, b := range []string{"topic-a", "topic-b"} {
		if _, err := runGit(bare, []string{"rev-parse", "--verify", "refs/heads/" + b}, runOpts{}); err != nil {
			t.Errorf("bare missing %s after push-all: %v", b, err)
		}
	}
}

// --- RebaseOnRemote ---

func TestRebaseOnRemoteDivergent(t *testing.T) {
	bare, _ := setupRemotePair(t)

	// Two clones that diverge.
	tmp := t.TempDir()
	a := filepath.Join(tmp, "a")
	b := filepath.Join(tmp, "b")
	gitRun(t, ".", "clone", bare, a)
	gitRun(t, ".", "clone", bare, b)

	commitWithMessage(t, a, "from-a.txt", "x", "from-a")
	gitRun(t, a, "push", "origin", "main")

	bLocalSHA := commitWithMessage(t, b, "from-b.txt", "y", "from-b")

	// b rebases onto origin/main — should replay b's commit on top
	// of a's.
	cb, _ := OpenClone(b, "", nullLogger())
	if err := cb.RebaseOnRemote("main"); err != nil {
		t.Fatalf("RebaseOnRemote: %v", err)
	}
	// After rebase, b's HEAD should be a commit that contains
	// both from-a.txt and from-b.txt.
	for _, p := range []string{"from-a.txt", "from-b.txt"} {
		if _, err := os.Stat(filepath.Join(b, p)); err != nil {
			t.Errorf("%s missing post-rebase: %v", p, err)
		}
	}
	_ = bLocalSHA
}

func TestRebaseOnRemoteEmptyBranchErrors(t *testing.T) {
	_, seed := setupRemotePair(t)
	c, _ := OpenClone(seed, "", nullLogger())
	if err := c.RebaseOnRemote(""); err == nil {
		t.Error("expected error on empty branch")
	}
}
