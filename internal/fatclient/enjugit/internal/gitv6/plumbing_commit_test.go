package gitv6

import (
	"strings"
	"sync"
	"testing"

	"github.com/go-git/go-git/v6/plumbing"
)

// TestPlumbingCommit_BasicOverlay covers the smallest case: take a
// base commit with one file, overlay one new file, verify the
// resulting commit has the right tree (both files), the right
// parent, and the right message.
func TestPlumbingCommit_BasicOverlay(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	baseSHA, _, _ := c.Head()

	newSHA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files: []FileWrite{
			{RepoRelPath: "data/result.md", Content: []byte("hello world\n")},
		},
		Message:     "test plumbing commit\n",
		AuthorName:  "Tester",
		AuthorEmail: "tester@example.com",
	})
	if err != nil {
		t.Fatalf("PlumbingCommit: %v", err)
	}
	if newSHA == "" {
		t.Fatal("PlumbingCommit returned empty SHA")
	}

	// Verify commit object exists and has the expected shape.
	commit, err := c.repo.CommitObject(plumbing.NewHash(newSHA))
	if err != nil {
		t.Fatalf("read new commit: %v", err)
	}
	if commit.Message != "test plumbing commit\n" {
		t.Errorf("message: got %q, want %q", commit.Message, "test plumbing commit\n")
	}
	if commit.Author.Name != "Tester" || commit.Author.Email != "tester@example.com" {
		t.Errorf("author: got %s <%s>, want Tester <tester@example.com>",
			commit.Author.Name, commit.Author.Email)
	}
	if len(commit.ParentHashes) != 1 || commit.ParentHashes[0].String() != baseSHA {
		t.Errorf("parent: got %v, want [%s]", commit.ParentHashes, baseSHA)
	}

	// Tree should contain README.md (from base) AND data/result.md (overlay).
	tree, err := commit.Tree()
	if err != nil {
		t.Fatalf("read commit tree: %v", err)
	}
	hasREADME := false
	hasDataDir := false
	for _, e := range tree.Entries {
		if e.Name == "README.md" {
			hasREADME = true
		}
		if e.Name == "data" {
			hasDataDir = true
		}
	}
	if !hasREADME {
		t.Error("tree missing README.md (base content not preserved)")
	}
	if !hasDataDir {
		t.Error("tree missing data/ subdir (overlay not applied)")
	}

	// Drill into data/ to confirm result.md is there with right content.
	dataTree, err := tree.Tree("data")
	if err != nil {
		t.Fatalf("read data subtree: %v", err)
	}
	resultEntry, err := dataTree.File("result.md")
	if err != nil {
		t.Fatalf("read data/result.md: %v", err)
	}
	gotContent, err := resultEntry.Contents()
	if err != nil {
		t.Fatalf("read result.md contents: %v", err)
	}
	if gotContent != "hello world\n" {
		t.Errorf("data/result.md content: got %q, want %q", gotContent, "hello world\n")
	}

	// PlumbingCommit must NOT have updated any ref. HEAD should
	// still point at the base commit.
	headSHA, _, _ := c.Head()
	if headSHA != baseSHA {
		t.Errorf("HEAD moved unexpectedly: was %s, now %s (PlumbingCommit must not touch HEAD)",
			baseSHA, headSHA)
	}
}

// TestPlumbingCommit_NestedPath covers a deeply-nested path like
// "a/b/c/file.md" — the buildNestedTree algorithm must synthesize
// all intermediate dirs.
func TestPlumbingCommit_NestedPath(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	baseSHA, _, _ := c.Head()

	newSHA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files: []FileWrite{
			{RepoRelPath: "a/b/c/file.md", Content: []byte("deep")},
		},
		Message: "nested\n",
	})
	if err != nil {
		t.Fatalf("PlumbingCommit: %v", err)
	}
	commit, _ := c.repo.CommitObject(plumbing.NewHash(newSHA))
	tree, _ := commit.Tree()
	a, err := tree.Tree("a")
	if err != nil {
		t.Fatalf("a/: %v", err)
	}
	b, err := a.Tree("b")
	if err != nil {
		t.Fatalf("a/b/: %v", err)
	}
	c2, err := b.Tree("c")
	if err != nil {
		t.Fatalf("a/b/c/: %v", err)
	}
	f, err := c2.File("file.md")
	if err != nil {
		t.Fatalf("a/b/c/file.md: %v", err)
	}
	got, _ := f.Contents()
	if got != "deep" {
		t.Errorf("content: got %q, want %q", got, "deep")
	}
}

// TestPlumbingCommit_OverwritesBaseFile verifies overlay semantics:
// when a FileWrite path already exists in the base tree, the
// overlay replaces it.
func TestPlumbingCommit_OverwritesBaseFile(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	baseSHA, _, _ := c.Head()

	newSHA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files: []FileWrite{
			// README.md exists in base (from seed); replace it.
			{RepoRelPath: "README.md", Content: []byte("new readme content\n")},
		},
		Message: "overwrite\n",
	})
	if err != nil {
		t.Fatalf("PlumbingCommit: %v", err)
	}
	commit, _ := c.repo.CommitObject(plumbing.NewHash(newSHA))
	tree, _ := commit.Tree()
	f, err := tree.File("README.md")
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	got, _ := f.Contents()
	if got != "new readme content\n" {
		t.Errorf("README.md content: got %q, want %q", got, "new readme content\n")
	}
}

// TestUpdateRef_CreatesAndAdvancesBranch covers the basic ref-update
// cases: create a new branch, advance an existing one, CAS race.
func TestUpdateRef_CreatesAndAdvancesBranch(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	baseSHA, _, _ := c.Head()

	commitA, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: baseSHA,
		Files:   []FileWrite{{RepoRelPath: "a.txt", Content: []byte("A")}},
		Message: "A\n",
	})
	if err != nil {
		t.Fatalf("commit A: %v", err)
	}

	// Create a new branch pointing at commitA. expectedOldSHA = ""
	// because the ref doesn't exist yet.
	if err := c.UpdateRef("topic-1", commitA, ""); err != nil {
		t.Fatalf("UpdateRef create: %v", err)
	}
	got, err := c.LocalBranchHash("topic-1")
	if err != nil {
		t.Fatalf("LocalBranchHash: %v", err)
	}
	if got != commitA {
		t.Errorf("after create: got %s, want %s", got, commitA)
	}

	// Advance topic-1 to a new commit, with CAS = current value.
	commitB, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: commitA,
		Files:   []FileWrite{{RepoRelPath: "b.txt", Content: []byte("B")}},
		Message: "B\n",
	})
	if err != nil {
		t.Fatalf("commit B: %v", err)
	}
	if err := c.UpdateRef("topic-1", commitB, commitA); err != nil {
		t.Fatalf("UpdateRef CAS-advance: %v", err)
	}
	got, _ = c.LocalBranchHash("topic-1")
	if got != commitB {
		t.Errorf("after CAS advance: got %s, want %s", got, commitB)
	}

	// CAS-advance with stale expected SHA should fail without
	// changing the ref.
	commitC, err := c.PlumbingCommit(PlumbingCommitRequest{
		BaseSHA: commitB,
		Files:   []FileWrite{{RepoRelPath: "c.txt", Content: []byte("C")}},
		Message: "C\n",
	})
	if err != nil {
		t.Fatalf("commit C: %v", err)
	}
	if err := c.UpdateRef("topic-1", commitC, commitA); err == nil {
		t.Error("expected CAS failure with stale expected SHA, got nil")
	} else if !strings.Contains(err.Error(), "CAS check failed") {
		t.Errorf("expected CAS-failure error, got: %v", err)
	}
	got, _ = c.LocalBranchHash("topic-1")
	if got != commitB {
		t.Errorf("after failed CAS: ref should be unchanged at %s, got %s", commitB, got)
	}
}

// TestPlumbingCommit_ConcurrentDistinctBranches is the smoke test
// for the parallel-compute target case: N goroutines each build a
// commit + update their own branch ref. Verifies all N commits
// are stored and each branch has the right SHA.
func TestPlumbingCommit_ConcurrentDistinctBranches(t *testing.T) {
	const N = 8

	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	baseSHA, _, _ := c.Head()

	type result struct {
		branch string
		sha    string
		err    error
	}
	results := make(chan result, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			branchName := "task-" + string(rune('A'+idx))
			path := "tasks/" + branchName + "/result.md"
			content := []byte("output of " + branchName)

			sha, err := c.PlumbingCommit(PlumbingCommitRequest{
				BaseSHA: baseSHA,
				Files:   []FileWrite{{RepoRelPath: path, Content: content}},
				Message: "task " + branchName + "\n",
			})
			if err != nil {
				results <- result{branch: branchName, err: err}
				return
			}
			if err := c.UpdateRef(branchName, sha, ""); err != nil {
				results <- result{branch: branchName, err: err}
				return
			}
			results <- result{branch: branchName, sha: sha}
		}(i)
	}
	wg.Wait()
	close(results)

	got := map[string]string{} // branch → sha
	for r := range results {
		if r.err != nil {
			t.Errorf("%s: %v", r.branch, r.err)
			continue
		}
		got[r.branch] = r.sha
	}
	if len(got) != N {
		t.Fatalf("expected %d successful results, got %d", N, len(got))
	}

	// Verify each branch ref points at the right commit and the
	// commit's tree carries that branch's file.
	for branch, sha := range got {
		gotRef, err := c.LocalBranchHash(branch)
		if err != nil {
			t.Errorf("%s: read ref: %v", branch, err)
			continue
		}
		if gotRef != sha {
			t.Errorf("%s: ref points at %s, want %s", branch, gotRef, sha)
		}

		commit, _ := c.repo.CommitObject(plumbing.NewHash(sha))
		tree, _ := commit.Tree()
		// File should be at tasks/<branch>/result.md.
		tasksTree, err := tree.Tree("tasks")
		if err != nil {
			t.Errorf("%s: missing tasks/ subtree: %v", branch, err)
			continue
		}
		branchTree, err := tasksTree.Tree(branch)
		if err != nil {
			t.Errorf("%s: missing tasks/%s/: %v", branch, branch, err)
			continue
		}
		f, err := branchTree.File("result.md")
		if err != nil {
			t.Errorf("%s: missing result.md: %v", branch, err)
			continue
		}
		c, _ := f.Contents()
		want := "output of " + branch
		if c != want {
			t.Errorf("%s: result.md content: got %q, want %q", branch, c, want)
		}
	}
}
