package enjugit

import (
	"os"
	"path/filepath"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestSubmitComputeTaskResult_HappyPath covers the basic case:
// submit one compute task, verify topic branch + commit + push.
func TestSubmitComputeTaskResult_HappyPath(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	res, err := wf.SubmitComputeTaskResult(SubmitRequest{
		TaskID:    "42:1:fetch_a",
		IterSeq:   1,
		RunSeq:    1,
		RunSlug:   "load-test",
		TaskDef:   "fetch_a",
		RunBranch: "main", // base for the topic-branch fork
		Citizen:   Identity{Name: "tamer", Email: "tamer@example.com"},
		Files: []FileWrite{
			{
				RepoRelPath: "data/fetch_a/result.md",
				Content:     []byte("a's output\n"),
			},
		},
	})
	if err != nil {
		t.Fatalf("SubmitComputeTaskResult: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("empty commit SHA")
	}
	expectedBranch := wf.convs.BranchName(1, "load-test", "fetch_a", "", 1)
	if res.BranchName != expectedBranch {
		t.Errorf("BranchName: got %s, want %s", res.BranchName, expectedBranch)
	}

	// Verify topic branch was pushed to bare with the right commit.
	verifyDir := t.TempDir()
	verifyRepo, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{
		URL:           bare,
		ReferenceName: plumbing.NewBranchReferenceName(expectedBranch),
		SingleBranch:  true,
	})
	if err != nil {
		t.Fatalf("verify clone of topic branch: %v", err)
	}
	verifyHead, err := verifyRepo.Head()
	if err != nil {
		t.Fatalf("verify head: %v", err)
	}
	if verifyHead.Hash().String() != res.CommitSHA {
		t.Errorf("topic branch on bare: got %s, want %s",
			verifyHead.Hash().String(), res.CommitSHA)
	}

	// File should be there.
	bytes, err := readFileFromGogitWorktree(verifyDir, "data/fetch_a/result.md")
	if err != nil {
		t.Fatalf("read file: %v", err)
	}
	if string(bytes) != "a's output\n" {
		t.Errorf("file content: got %q, want %q", string(bytes), "a's output\n")
	}
}

// TestSubmitComputeTaskResult_DoesNotMoveRunBranch is the
// invariant: submitting a compute task creates a topic branch
// pointing at the new commit but leaves the run branch tip
// unchanged. Auto-merge into the run branch happens later
// (by the existing parallel-merge / auto-merge path on coord
// acceptance), not as part of submit.
func TestSubmitComputeTaskResult_DoesNotMoveRunBranch(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	mainBefore, err := wf.git.LocalBranchHash("main")
	if err != nil {
		t.Fatalf("read main before: %v", err)
	}

	_, err = wf.SubmitComputeTaskResult(SubmitRequest{
		TaskID:    "42:1:t1",
		IterSeq:   1,
		RunSeq:    1,
		RunSlug:   "iso-test",
		TaskDef:   "t1",
		RunBranch: "main",
		Citizen:   Identity{Name: "tamer", Email: "tamer@example.com"},
		Files: []FileWrite{
			{RepoRelPath: "out/t1.md", Content: []byte("x")},
		},
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	mainAfter, err := wf.git.LocalBranchHash("main")
	if err != nil {
		t.Fatalf("read main after: %v", err)
	}
	if mainAfter != mainBefore {
		t.Errorf("run branch (main) tip moved unexpectedly: was %s, now %s "+
			"(SubmitComputeTaskResult must NOT advance the run branch — auto-merge does that later)",
			mainBefore, mainAfter)
	}
}

// TestSubmitComputeTaskResult_ConcurrentParallelTasks is the
// integration test that proves the parallel-compute fix works:
// 4 goroutines each submit a compute task on the same Workflow,
// concurrently, targeting different topic branches. All 4 must
// succeed, each commit must land on its own topic branch ref,
// and each ref must be pushed to bare.
//
// This is the test case that the OLD design (each goroutine
// opened its own Workspace, raced on .git/config) failed flakily.
// The plumbing path eliminates the race because no checkout
// happens, no shared HEAD, no shared index.
func TestSubmitComputeTaskResult_ConcurrentParallelTasks(t *testing.T) {
	const N = 4

	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	type result struct {
		taskDef string
		sha     string
		branch  string
		err     error
	}
	results := make(chan result, N)

	var wg sync.WaitGroup
	for i := 0; i < N; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			taskDef := "task_" + string(rune('a'+idx))
			res, err := wf.SubmitComputeTaskResult(SubmitRequest{
				TaskID:    "42:1:" + taskDef,
				IterSeq:   1,
				RunSeq:    1,
				RunSlug:   "parallel-test",
				TaskDef:   taskDef,
				RunBranch: "main",
				Citizen:   Identity{Name: "tamer", Email: "tamer@example.com"},
				Files: []FileWrite{
					{RepoRelPath: "out/" + taskDef + "/result.md",
						Content: []byte("output of " + taskDef)},
				},
			})
			if err != nil {
				results <- result{taskDef: taskDef, err: err}
				return
			}
			results <- result{taskDef: taskDef, sha: res.CommitSHA, branch: res.BranchName}
		}(i)
	}
	wg.Wait()
	close(results)

	got := map[string]result{}
	for r := range results {
		if r.err != nil {
			t.Errorf("%s: %v", r.taskDef, r.err)
			continue
		}
		got[r.taskDef] = r
	}
	if len(got) != N {
		t.Fatalf("expected %d successful submits, got %d", N, len(got))
	}

	// All commit SHAs must be distinct (each task produced its own).
	seen := map[string]string{} // sha → taskDef
	for taskDef, r := range got {
		if other, ok := seen[r.sha]; ok {
			t.Errorf("collision: %s and %s both produced commit %s", other, taskDef, r.sha)
		}
		seen[r.sha] = taskDef
	}

	// Each topic branch should be on bare and contain its file.
	for taskDef, r := range got {
		verifyDir := t.TempDir()
		_, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{
			URL:           bare,
			ReferenceName: plumbing.NewBranchReferenceName(r.branch),
			SingleBranch:  true,
		})
		if err != nil {
			t.Errorf("%s: clone topic branch %s: %v", taskDef, r.branch, err)
			continue
		}
		bytes, err := readFileFromGogitWorktree(verifyDir, "out/"+taskDef+"/result.md")
		if err != nil {
			t.Errorf("%s: read result: %v", taskDef, err)
			continue
		}
		want := "output of " + taskDef
		if string(bytes) != want {
			t.Errorf("%s: content: got %q, want %q", taskDef, string(bytes), want)
		}
	}
}

// readFileFromGogitWorktree reads a file from a checked-out
// worktree, used by the verify-clone parts of these tests.
func readFileFromGogitWorktree(dir, relPath string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dir, relPath))
}
