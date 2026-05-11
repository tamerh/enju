package gitcli

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// --- CommitFiles ---

func TestCommitFilesNewFiles(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	// Need an initial commit so HEAD resolves; CommitFiles
	// commits onto the current branch which must exist as a
	// ref.
	seedCommitOnMain(t, dir, "seed.txt", "seed")

	c, _ := OpenClone(dir, "", nullLogger())
	res, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "a.txt", Content: []byte("aa")},
			{RepoRelPath: "sub/b.txt", Content: []byte("bb")},
		},
		Message:     "test: add a and b",
		AuthorName:  "Tester",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if res.NoOp {
		t.Error("NoOp=true on fresh files")
	}
	if !isHexSHA(res.SHA) {
		t.Errorf("SHA = %q, want 40-hex", res.SHA)
	}
	// Verify both files in the commit.
	for _, p := range []string{"a.txt", "sub/b.txt"} {
		out := strings.TrimSpace(gitRun(t, dir, "cat-file", "-e", "HEAD:"+p))
		_ = out // ok if cat-file -e didn't error
	}
	// Verify author.
	out := strings.TrimSpace(gitRun(t, dir, "log", "-1", "--format=%an <%ae>"))
	if !strings.Contains(out, "Tester") || !strings.Contains(out, "test@example.com") {
		t.Errorf("author = %q, want Tester <test@example.com>", out)
	}
}

func TestCommitFilesNoOpWhenContentIdentical(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "a.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	preSHA, _ := c.headSHALocked()
	res, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "a.txt", Content: []byte("x")}, // same as HEAD
		},
		Message:     "test: idempotent",
		AuthorName:  "T",
		AuthorEmail: "t@x",
	})
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if !res.NoOp {
		t.Error("expected NoOp=true on identical content")
	}
	if res.SHA != preSHA {
		t.Errorf("NoOp SHA = %s, want HEAD %s", res.SHA, preSHA)
	}
	// HEAD should still be at preSHA — no new commit.
	postSHA, _ := c.headSHALocked()
	if postSHA != preSHA {
		t.Errorf("HEAD moved on NoOp: %s -> %s", preSHA, postSHA)
	}
}

func TestCommitFilesStagePathsSubsetEnforced(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "seed.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	_, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "a.txt", Content: []byte("a")},
		},
		StagePaths: []string{"a.txt", "b.txt"}, // b.txt NOT in Files
		Message:    "x",
	})
	if err == nil {
		t.Error("expected error for StagePath not in Files")
	}
}

func TestCommitFilesPlaceholderAuthorWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "seed.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	_, err := c.CommitFiles(CommitRequest{
		Files:   []FileWrite{{RepoRelPath: "a.txt", Content: []byte("a")}},
		Message: "x",
		// No AuthorName/AuthorEmail.
	})
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	out := strings.TrimSpace(gitRun(t, dir, "log", "-1", "--format=%ae"))
	if out != "enju-git@localhost" {
		t.Errorf("placeholder email = %q, want enju-git@localhost", out)
	}
}

func TestCommitFilesPreservesExecutableMode(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "seed.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	_, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "run.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755},
		},
		Message: "x",
	})
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	out := strings.TrimSpace(gitRun(t, dir, "ls-tree", "HEAD", "run.sh"))
	if !strings.HasPrefix(out, "100755") {
		t.Errorf("expected mode 100755, got: %q", out)
	}
}

// --- PlumbingCommit ---

func TestPlumbingCommitOnBase(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	baseSHA := seedCommitOnMain(t, dir, "a.txt", "from-base")

	c, _ := OpenClone(dir, "", nullLogger())
	preHEAD, _ := c.headSHALocked()
	commitSHA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files: []FileWrite{
			{RepoRelPath: "b.txt", Content: []byte("from-plumbing")},
		},
		Message:     "plumbing test",
		AuthorName:  "P",
		AuthorEmail: "p@x",
	})
	if err != nil {
		t.Fatalf("PlumbingCommit: %v", err)
	}
	if !isHexSHA(commitSHA) {
		t.Errorf("commit SHA = %q", commitSHA)
	}
	// HEAD must not have moved.
	postHEAD, _ := c.headSHALocked()
	if postHEAD != preHEAD {
		t.Errorf("HEAD moved: %s -> %s (plumbing must not advance HEAD)", preHEAD, postHEAD)
	}
	// Verify the new commit's tree contains both files.
	for _, p := range []string{"a.txt", "b.txt"} {
		out := strings.TrimSpace(gitRun(t, dir, "cat-file", "-e", commitSHA+":"+p))
		_ = out
	}
	// And the parent is baseSHA.
	parents := strings.TrimSpace(gitRun(t, dir, "rev-list", "--parents", "-n1", commitSHA))
	if !strings.Contains(parents, baseSHA) {
		t.Errorf("parent missing: %q, want contains %s", parents, baseSHA)
	}
}

func TestPlumbingCommitOrphan(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "seed.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	commitSHA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: "", // orphan
		Files: []FileWrite{
			{RepoRelPath: "only.txt", Content: []byte("standalone")},
		},
		Message: "orphan",
	})
	if err != nil {
		t.Fatalf("PlumbingCommit: %v", err)
	}
	// Should have no parents.
	parents := strings.TrimSpace(gitRun(t, dir, "rev-list", "--parents", "-n1", commitSHA))
	// Format: "<commit> [<parent>...]"; orphan has no parent.
	if strings.Count(parents, " ") != 0 {
		t.Errorf("orphan should have no parents, got: %q", parents)
	}
}

func TestPlumbingCommitErrCommitNotFoundBaseSHA(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	seedCommitOnMain(t, dir, "seed.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	_, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: bogus,
		Files:   []FileWrite{{RepoRelPath: "a.txt", Content: []byte("a")}},
		Message: "x",
	})
	if err == nil {
		t.Error("expected error for missing base SHA")
	}
}

func TestPlumbingCommitDoesNotMutateIndexOrWorktree(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	baseSHA := seedCommitOnMain(t, dir, "a.txt", "x")

	// Make the worktree dirty before plumbing-commit.
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("dirty"), 0o644); err != nil {
		t.Fatal(err)
	}
	preIndexBefore := strings.TrimSpace(gitRun(t, dir, "diff", "--cached"))

	c, _ := OpenClone(dir, "", nullLogger())
	_, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files:   []FileWrite{{RepoRelPath: "new.txt", Content: []byte("new")}},
		Message: "x",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Worktree dirty content must be unchanged.
	got, _ := os.ReadFile(filepath.Join(dir, "a.txt"))
	if string(got) != "dirty" {
		t.Errorf("worktree mutated: %q", got)
	}
	// Index unchanged.
	postIndex := strings.TrimSpace(gitRun(t, dir, "diff", "--cached"))
	if postIndex != preIndexBefore {
		t.Errorf("index mutated:\n  before: %q\n  after:  %q", preIndexBefore, postIndex)
	}
	// HEAD unchanged.
	postHEAD := strings.TrimSpace(gitRun(t, dir, "rev-parse", "HEAD"))
	if postHEAD != baseSHA {
		t.Errorf("HEAD advanced: %s -> %s", baseSHA, postHEAD)
	}
}

func TestPlumbingCommitExecutableMode(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	baseSHA := seedCommitOnMain(t, dir, "seed.txt", "x")

	c, _ := OpenClone(dir, "", nullLogger())
	commitSHA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files: []FileWrite{
			{RepoRelPath: "run.sh", Content: []byte("#!/bin/sh\n"), Mode: 0o755},
		},
		Message: "x",
	})
	if err != nil {
		t.Fatalf("PlumbingCommit: %v", err)
	}
	out := strings.TrimSpace(gitRun(t, dir, "ls-tree", commitSHA, "run.sh"))
	if !strings.HasPrefix(out, "100755") {
		t.Errorf("expected mode 100755, got: %q", out)
	}
}

// --- MergeFFOrFail ---

func TestMergeFFOrFailFastForwardSucceeds(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "first")
	gitRun(t, dir, "branch", "topic-x")
	topicSHA := commitWithMessage(t, dir, "b.txt", "2", "second")
	// Now: main at topicSHA, topic-x at first.
	// Set main back to first so we can FF main → topic-x's tip.
	mainSHA := strings.TrimSpace(gitRun(t, dir, "rev-parse", "topic-x"))
	gitRun(t, dir, "branch", "-f", "topic-x", topicSHA)
	gitRun(t, dir, "checkout", "topic-x")
	gitRun(t, dir, "branch", "-f", "main", mainSHA)

	c, _ := OpenClone(dir, "", nullLogger())
	newTip, err := c.MergeFFOrFail("main", "topic-x")
	if err != nil {
		t.Fatalf("MergeFFOrFail: %v", err)
	}
	if newTip != topicSHA {
		t.Errorf("newTip = %s, want %s", newTip, topicSHA)
	}
	// Verify main now points at topicSHA.
	gotMain := strings.TrimSpace(gitRun(t, dir, "rev-parse", "refs/heads/main"))
	if gotMain != topicSHA {
		t.Errorf("main = %s, want %s", gotMain, topicSHA)
	}
}

func TestMergeFFOrFailIdenticalNoop(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	sha := commitWithMessage(t, dir, "a.txt", "1", "first")
	gitRun(t, dir, "branch", "topic-x") // both at same SHA

	c, _ := OpenClone(dir, "", nullLogger())
	newTip, err := c.MergeFFOrFail("main", "topic-x")
	if err != nil {
		t.Fatalf("MergeFFOrFail: %v", err)
	}
	if newTip != sha {
		t.Errorf("newTip = %s, want %s", newTip, sha)
	}
}

func TestMergeFFOrFailNonFFReturnsErrPushNonFF(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "first")
	gitRun(t, dir, "branch", "topic-x")
	// Diverge: main and topic-x both add different commits.
	gitRun(t, dir, "checkout", "topic-x")
	commitWithMessage(t, dir, "t.txt", "t", "topic side")
	gitRun(t, dir, "checkout", "main")
	commitWithMessage(t, dir, "m.txt", "m", "main side")

	c, _ := OpenClone(dir, "", nullLogger())
	_, err := c.MergeFFOrFail("main", "topic-x")
	if !errors.Is(err, ErrPushNonFF) {
		t.Errorf("expected ErrPushNonFF, got %v", err)
	}
}

func TestMergeFFOrFailErrRefNotFound(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "a.txt", "1", "first")

	c, _ := OpenClone(dir, "", nullLogger())
	_, err := c.MergeFFOrFail("never-existed", "main")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound on missing target, got %v", err)
	}
	_, err = c.MergeFFOrFail("main", "never-existed")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound on missing source, got %v", err)
	}
}

// --- MergeWithCommit ---

func TestMergeWithCommitCleanMerge(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitWithMessage(t, dir, "common.txt", "c", "common")
	gitRun(t, dir, "branch", "topic-x")
	gitRun(t, dir, "checkout", "topic-x")
	commitWithMessage(t, dir, "topic.txt", "t", "topic")
	gitRun(t, dir, "checkout", "main")
	commitWithMessage(t, dir, "main.txt", "m", "main")

	c, _ := OpenClone(dir, "", nullLogger())
	mergeSHA, err := c.MergeWithCommit("main", "topic-x", "Merge topic", "T", "t@x")
	if err != nil {
		t.Fatalf("MergeWithCommit: %v", err)
	}
	if !isHexSHA(mergeSHA) {
		t.Errorf("merge SHA = %q", mergeSHA)
	}
	// main should now point at mergeSHA.
	got := strings.TrimSpace(gitRun(t, dir, "rev-parse", "refs/heads/main"))
	if got != mergeSHA {
		t.Errorf("main = %s, want %s", got, mergeSHA)
	}
	// Merge commit has 2 parents.
	parents := strings.TrimSpace(gitRun(t, dir, "rev-list", "--parents", "-n1", mergeSHA))
	// Format: "<commit> <p1> <p2>"
	if strings.Count(parents, " ") != 2 {
		t.Errorf("expected 2 parents, got: %q", parents)
	}
	// Merge tree has all three files.
	for _, p := range []string{"common.txt", "topic.txt", "main.txt"} {
		_, err := runGit(dir, []string{"cat-file", "-e", mergeSHA + ":" + p}, runOpts{})
		if err != nil {
			t.Errorf("merge tree missing %s: %v", p, err)
		}
	}
}

func TestMergeWithCommitConflictReturnsErrMergeConflict(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	// Both branches modify the same file in incompatible ways.
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("base\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "add", "f.txt")
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-m", "base")

	gitRun(t, dir, "branch", "topic-x")
	gitRun(t, dir, "checkout", "topic-x")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("topic\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-am", "topic")

	gitRun(t, dir, "checkout", "main")
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("main\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	gitRun(t, dir, "-c", "user.name=t", "-c", "user.email=t@x", "commit", "-am", "main")

	c, _ := OpenClone(dir, "", nullLogger())
	preMain := strings.TrimSpace(gitRun(t, dir, "rev-parse", "refs/heads/main"))

	_, err := c.MergeWithCommit("main", "topic-x", "Merge topic", "T", "t@x")
	if !errors.Is(err, ErrMergeConflict) {
		t.Errorf("expected ErrMergeConflict, got %v", err)
	}
	var conflictErr *ErrConflict
	if errors.As(err, &conflictErr) {
		found := false
		for _, p := range conflictErr.Paths {
			if p == "f.txt" {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected f.txt in conflict paths, got: %v", conflictErr.Paths)
		}
	} else {
		t.Errorf("error not *ErrConflict: %v", err)
	}
	// main ref must NOT have advanced.
	postMain := strings.TrimSpace(gitRun(t, dir, "rev-parse", "refs/heads/main"))
	if postMain != preMain {
		t.Errorf("main advanced despite conflict: %s -> %s", preMain, postMain)
	}
}
