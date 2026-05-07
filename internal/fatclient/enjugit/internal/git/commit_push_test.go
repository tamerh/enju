package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
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
