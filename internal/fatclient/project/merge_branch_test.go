package project

// Parallel-merge Phase 1 contract: MergeBranchToCommit prefers
// the FF fast path (no merge commit when the run branch hasn't
// advanced past the topic's fork point) and falls back to a
// real merge commit when a sibling has already landed. On a
// content conflict, the merge is aborted cleanly and
// ErrMergeConflict is returned with the conflicting files.
//
// These tests pin the three observable shapes:
//   - sequential single-task case: linear history, no merge bubbles.
//   - parallel siblings with disjoint writes: clean merge commit.
//   - parallel siblings with overlapping writes: ErrMergeConflict.

import (
	"errors"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// makeTopicCommit creates a topic branch forked from baseBranch
// and pushes one file to it. Returns the resulting commit SHA.
// Used to set up the parallel-sibling shape in tests.
func makeTopicCommit(t *testing.T, p *Clone, baseBranch, topicBranch, filePath, content string) string {
	t.Helper()
	p.Lock()
	defer p.Unlock()
	res, err := p.SubmitTaskResult(SubmitRequest{
		TaskID:     "task-" + topicBranch,
		Username:   "tester",
		Branch:     topicBranch,
		BaseBranch: baseBranch,
		Files:      []FileWrite{{RepoRelPath: filePath, Content: []byte(content)}},
	})
	if err != nil {
		t.Fatalf("submit on %s: %v", topicBranch, err)
	}
	return res.CommitSHA
}

// TestMergeBranchToCommit_FFFastPath pins the sequential case.
// One topic forked from main, no sibling, FF merge advances main
// without producing a merge commit. Pre-parallel-merge behavior
// must be preserved exactly.
func TestMergeBranchToCommit_FFFastPath(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	topicSHA := makeTopicCommit(t, proj, "main", "topic-solo", "out/solo.md", "solo")

	proj.Lock()
	defer proj.Unlock()
	if err := proj.MergeBranchToCommit("main", topicSHA, "topic-solo", MergeAuthor{}); err != nil {
		t.Fatalf("MergeBranchToCommit FF: %v", err)
	}

	// main on the bare remote should be at topicSHA exactly,
	// and the topic commit's parent count should be 1 (FF means
	// the same commit ends up on main — no new merge commit).
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	mainRef, err := bareRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("read main on bare: %v", err)
	}
	if mainRef.Hash().String() != topicSHA {
		t.Errorf("FF merge: bare main = %s, want topic SHA %s", mainRef.Hash(), topicSHA)
	}
	tipCommit, err := bareRepo.CommitObject(mainRef.Hash())
	if err != nil {
		t.Fatalf("load tip commit: %v", err)
	}
	if got := tipCommit.NumParents(); got != 1 {
		t.Errorf("FF tip should have 1 parent, got %d", got)
	}
}

// TestMergeBranchToCommit_NonFFDisjointWrites is the load-bearing
// parallel-siblings test. Two topics fork from the same main
// base; the first merges FF, the second merges as a real merge
// commit because main has advanced past its fork point. Both
// files end up on main and the merge tip has two parents.
func TestMergeBranchToCommit_NonFFDisjointWrites(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Create both topics BEFORE any merge so they share the
	// same fork point (main = seed). After topic-a merges, main
	// advances past topic-b's base, forcing the second merge
	// onto the non-FF path.
	topicA := makeTopicCommit(t, proj, "main", "topic-a", "out/a.md", "alice")
	topicB := makeTopicCommit(t, proj, "main", "topic-b", "out/b.md", "bob")

	proj.Lock()
	defer proj.Unlock()

	if err := proj.MergeBranchToCommit("main", topicA, "topic-a",
		MergeAuthor{Name: "Alice", Email: "alice@example.com", TaskID: "task-a"}); err != nil {
		t.Fatalf("first merge (FF): %v", err)
	}
	if err := proj.MergeBranchToCommit("main", topicB, "topic-b",
		MergeAuthor{Name: "Bob", Email: "bob@example.com", TaskID: "task-b"}); err != nil {
		t.Fatalf("second merge (non-FF): %v", err)
	}

	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	mainRef, err := bareRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	tip, err := bareRepo.CommitObject(mainRef.Hash())
	if err != nil {
		t.Fatalf("load tip: %v", err)
	}
	if got := tip.NumParents(); got != 2 {
		t.Fatalf("merge tip should have 2 parents, got %d (msg: %q)", got, tip.Message)
	}
	// Parent[0] is the run-branch tip we merged onto (topic-a's
	// SHA, which FF'd to main). Parent[1] is topic-b's SHA.
	if tip.ParentHashes[0].String() != topicA {
		t.Errorf("merge parent[0] = %s, want topic-a SHA %s", tip.ParentHashes[0], topicA)
	}
	if tip.ParentHashes[1].String() != topicB {
		t.Errorf("merge parent[1] = %s, want topic-b SHA %s", tip.ParentHashes[1], topicB)
	}
	// Trailer + author sanity: the merge was triggered by Bob's
	// accept of topic-b, so the merge commit should be authored
	// by Bob and the message should carry the Enju-Merge: auto
	// trailer.
	if tip.Author.Email != "bob@example.com" {
		t.Errorf("merge author email = %q, want bob@example.com", tip.Author.Email)
	}
	wantTrailers := []string{
		"Enju-Merge: auto",
		"Enju-Merged-Topic: topic-b",
		"Enju-Merged-Run: main",
		"Enju-Triggered-By: task-b",
	}
	for _, want := range wantTrailers {
		if !strings.Contains(tip.Message, want) {
			t.Errorf("merge message missing trailer %q\nfull message:\n%s", want, tip.Message)
		}
	}

	// Verify both files reachable from main's tip — the merge
	// commit itself carries both contributions.
	tree, err := tip.Tree()
	if err != nil {
		t.Fatalf("tree: %v", err)
	}
	for _, p := range []string{"out/a.md", "out/b.md"} {
		if _, err := tree.File(p); err != nil {
			t.Errorf("file %q missing from merge tip tree: %v", p, err)
		}
	}
}

// TestMergeBranchToCommit_NonFFOverlappingWrites_ConflictReturned
// pins the conflict-detection contract. Two parallel siblings
// write to the same path with different content; the second
// merge must abort cleanly and return ErrMergeConflict listing
// the file. The run branch ref must be unchanged (still at the
// first topic's SHA).
func TestMergeBranchToCommit_NonFFOverlappingWrites_ConflictReturned(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)
	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	topicA := makeTopicCommit(t, proj, "main", "topic-a", "shared/conflict.md", "alice's version\n")
	topicB := makeTopicCommit(t, proj, "main", "topic-b", "shared/conflict.md", "bob's version\n")

	proj.Lock()
	defer proj.Unlock()

	if err := proj.MergeBranchToCommit("main", topicA, "topic-a",
		MergeAuthor{Name: "Alice", Email: "alice@example.com", TaskID: "task-a"}); err != nil {
		t.Fatalf("first merge (FF): %v", err)
	}

	mergeErr := proj.MergeBranchToCommit("main", topicB, "topic-b",
		MergeAuthor{Name: "Bob", Email: "bob@example.com", TaskID: "task-b"})
	if mergeErr == nil {
		t.Fatal("non-FF merge with overlapping writes should have failed; got nil")
	}
	var conflictErr *ErrMergeConflict
	if !errors.As(mergeErr, &conflictErr) {
		t.Fatalf("expected *ErrMergeConflict, got %T: %v", mergeErr, mergeErr)
	}
	if conflictErr.Branch != "main" {
		t.Errorf("ConflictErr.Branch = %q, want main", conflictErr.Branch)
	}
	if conflictErr.TopicCommit != topicB {
		t.Errorf("ConflictErr.TopicCommit = %s, want %s", conflictErr.TopicCommit, topicB)
	}
	if conflictErr.RunTipCommit != topicA {
		t.Errorf("ConflictErr.RunTipCommit = %s, want %s", conflictErr.RunTipCommit, topicA)
	}
	if !containsString(conflictErr.ConflictFiles, "shared/conflict.md") {
		t.Errorf("ConflictErr.ConflictFiles = %v, want to include shared/conflict.md", conflictErr.ConflictFiles)
	}

	// Run branch unchanged: still at topicA both locally and on
	// the bare remote. Critical invariant — callers depend on
	// this to know the merge didn't half-land.
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	mainRef, err := bareRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	if mainRef.Hash().String() != topicA {
		t.Errorf("after conflict, bare main = %s, want unchanged at topic-a SHA %s",
			mainRef.Hash(), topicA)
	}
}

func containsString(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}
