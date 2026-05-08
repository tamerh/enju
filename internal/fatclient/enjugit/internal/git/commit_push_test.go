package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

func TestCommitFiles_HappyPath(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	res, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "src/foo.go", Content: []byte("package foo\n")},
		},
		Message:     "add foo",
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if !isHexSHA(res.SHA) {
		t.Errorf("expected SHA, got %q", res.SHA)
	}
	if res.NoOp {
		t.Error("first commit should not be NoOp")
	}
	// File must be on disk + readable from the new commit.
	body, found, _ := c.ReadFileAtCommit(res.SHA, "src/foo.go")
	if !found {
		t.Fatal("file not in commit")
	}
	if string(body) != "package foo\n" {
		t.Errorf("content mismatch: %q", body)
	}
}

func TestCommitFiles_NoOp(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// First commit.
	first, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "x.txt", Content: []byte("v1")},
		},
		Message: "first",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Same content again — must be no-op.
	second, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "x.txt", Content: []byte("v1")},
		},
		Message: "would be duplicate",
	})
	if err != nil {
		t.Fatalf("re-commit identical: %v", err)
	}
	if !second.NoOp {
		t.Error("expected NoOp=true on identical re-commit")
	}
	if second.SHA != first.SHA {
		t.Errorf("NoOp SHA should equal HEAD: got %s, want %s", second.SHA, first.SHA)
	}
}

func TestCommitFiles_StagePathsValidation(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	_, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "a.txt", Content: []byte("a")},
		},
		StagePaths: []string{"b.txt"}, // not in Files
		Message:    "bad",
	})
	if err == nil {
		t.Error("expected error for StagePath not in Files")
	}
}

func TestPush_HappyPath(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	res, err := c.CommitFiles(CommitRequest{
		Files:   []FileWrite{{RepoRelPath: "x.txt", Content: []byte("x")}},
		Message: "add x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Push("main"); err != nil {
		t.Fatalf("Push: %v", err)
	}
	// Verify by re-fetching from a fresh clone.
	other := freshClone(t, bare)
	body, found, _ := other.ReadFileAtCommit(res.SHA, "x.txt")
	if !found || string(body) != "x" {
		t.Errorf("pushed content not visible to fresh clone")
	}
}

func TestPush_RefNotFound(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	err := c.Push("nonexistent")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

func TestPush_NoRemote(t *testing.T) {
	// Init a local repo with no origin.
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "seed.txt"), []byte("s"), 0o644); err != nil {
		t.Fatal(err)
	}
	c, err := CloneOrInit(dir, "https://example.invalid/x.git", "", nullLogger())
	_ = c
	_ = err
	// Skip the no-remote case for now — CloneOrInit requires
	// a real source. The contract is exercised in higher-level
	// tests where solo projects exist.
	t.Skip("no-remote path tested via solo-project integration tests")
}

// TestPushAllRefs_PropagatesAllBranches — the load-bearing case
// for enju_set_project_remote: a clone holds N local branches
// (main + topics), and pointing it at a fresh bare must push all
// N up so the new remote is a complete mirror, not just origin/main.
func TestPushAllRefs_PropagatesAllBranches(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Land a commit on main.
	res, err := c.CommitFiles(CommitRequest{
		Files:   []FileWrite{{RepoRelPath: "main.txt", Content: []byte("m")}},
		Message: "main commit",
	})
	if err != nil {
		t.Fatal(err)
	}
	mainSHA := res.SHA

	// Create two topic branches at main's tip.
	if err := c.CreateBranchAt("topic-a", mainSHA); err != nil {
		t.Fatal(err)
	}
	if err := c.CreateBranchAt("topic-b", mainSHA); err != nil {
		t.Fatal(err)
	}

	// PushAllRefs ships everything in one call.
	if err := c.PushAllRefs(false); err != nil {
		t.Fatalf("PushAllRefs: %v", err)
	}

	// Verify by re-fetching from a fresh clone — all three branches
	// must be reachable.
	other := freshClone(t, bare)
	for _, br := range []string{"main", "topic-a", "topic-b"} {
		ref, err := other.repo.Reference(plumbing.NewRemoteReferenceName("origin", br), true)
		if err != nil {
			t.Errorf("origin/%s missing on fresh clone: %v", br, err)
			continue
		}
		if ref.Hash().String() != mainSHA {
			t.Errorf("origin/%s = %s, want %s", br, ref.Hash().String(), mainSHA)
		}
	}
	// State bookkeeping must update on success.
	if c.lastPushAt.IsZero() {
		t.Errorf("lastPushAt should be set after successful PushAllRefs")
	}
	if c.lastPushError != "" {
		t.Errorf("lastPushError should be empty after success, got %q", c.lastPushError)
	}
}

// TestPushAllRefs_NoOpIdempotent — second call is a no-op (already
// up to date) and must not error.
func TestPushAllRefs_NoOpIdempotent(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if err := c.PushAllRefs(false); err != nil {
		t.Fatalf("first PushAllRefs: %v", err)
	}
	if err := c.PushAllRefs(false); err != nil {
		t.Errorf("second PushAllRefs (no-op) should succeed, got: %v", err)
	}
}

func TestPushWithVerify_OK(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	res, err := c.CommitFiles(CommitRequest{
		Files:   []FileWrite{{RepoRelPath: "x.txt", Content: []byte("x")}},
		Message: "add x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := c.PushWithVerify("main", res.SHA); err != nil {
		t.Errorf("PushWithVerify: %v", err)
	}
}

func TestPushWithVerify_WrongExpected(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if _, err := c.CommitFiles(CommitRequest{
		Files:   []FileWrite{{RepoRelPath: "x.txt", Content: []byte("x")}},
		Message: "add x",
	}); err != nil {
		t.Fatal(err)
	}
	// Expect a SHA that won't match.
	err := c.PushWithVerify("main", "0000000000000000000000000000000000000000")
	if !errors.Is(err, ErrPushVerifyFailed) {
		t.Errorf("expected ErrPushVerifyFailed, got %v", err)
	}
}

// TestVerifyRemoteMatches_MissingRef pins the "remote ref doesn't
// exist at all" path inside verifyRemoteMatches — distinct from
// TestPushWithVerify_WrongExpected, which exercises the SHA-mismatch
// branch. Production saw silent-success failures where push reported
// ok but the branch never appeared on the bare; this test confirms
// verify catches that shape and returns an *ErrVerifyFailed with
// RemoteSHA="" so callers can distinguish "wrong content" from
// "ref vanished."
func TestVerifyRemoteMatches_MissingRef(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Make a local commit so we have a real SHA to verify, but
	// don't push the branch — the bare won't have it. This is
	// equivalent to the production "push went into the void"
	// shape.
	res, err := c.CommitFiles(CommitRequest{
		Files:   []FileWrite{{RepoRelPath: "x.txt", Content: []byte("x")}},
		Message: "add x",
	})
	if err != nil {
		t.Fatal(err)
	}
	c.CreateBranchAt("topic-vanish", res.SHA)

	vErr := c.verifyRemoteMatches("topic-vanish", res.SHA)
	if vErr == nil {
		t.Fatal("expected ErrVerifyFailed when remote ref is missing; got nil")
	}
	if !errors.Is(vErr, ErrPushVerifyFailed) {
		t.Errorf("expected ErrPushVerifyFailed in chain, got %v", vErr)
	}
	var typed *ErrVerifyFailed
	if !errors.As(vErr, &typed) {
		t.Fatalf("expected *ErrVerifyFailed, got %T: %v", vErr, vErr)
	}
	if typed.RemoteSHA != "" {
		t.Errorf("RemoteSHA = %q, want empty (missing) for a vanished ref", typed.RemoteSHA)
	}
	if typed.LocalSHA != res.SHA {
		t.Errorf("LocalSHA = %s, want %s", typed.LocalSHA, res.SHA)
	}
	if typed.Branch != "topic-vanish" {
		t.Errorf("Branch = %q, want topic-vanish", typed.Branch)
	}
}

func TestFetch_BringsDownNewBranches(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	a := freshClone(t, bare)
	b := freshClone(t, bare)

	// Alice creates a topic branch and pushes.
	headSHA, _, _ := a.Head()
	a.CreateBranchAt("topic/from-a", headSHA)
	a.Checkout("topic/from-a")
	commitOneFile(t, a, "topic.md", []byte("from a"))
	if err := a.Push("topic/from-a"); err != nil {
		t.Fatal(err)
	}

	// Bob fetches; the branch should be reachable as origin/topic/from-a.
	if err := b.Fetch(); err != nil {
		t.Fatalf("Fetch: %v", err)
	}
	if _, err := b.ResolveRef("topic/from-a"); err != nil {
		t.Errorf("topic/from-a not resolvable after fetch: %v", err)
	}
}
