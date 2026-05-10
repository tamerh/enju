package enjugit

// Parallel-merge integration test for Workflow.MergeAcceptedTopic.
// Reproduces the load-test failure where N concurrent
// MergeAcceptedTopic calls into the same target each report
// success but only some of their topic commits remain reachable
// from origin's target tip — i.e. silent commit loss.
//
// Root cause being pinned by this test: the *gitv6.Clone's
// reentrant flag was a plain bool on the struct, not goroutine-
// local. Once goroutine A entered WithLock and set reentrant=true,
// goroutines B/C/D's WithLock would see the flag and bypass the
// mutex entirely, running concurrently. That collapsed the
// merge-commit-push sequence's atomicity: parallel goroutines
// read the same target tip, built divergent merge commits, and
// last-writer-wins on the local ref + push.

import (
	"sync"
	"testing"
)

// TestMergeAcceptedTopic_FourParallelTopicsAllReachable forks
// four topic branches from the same main base, then fires four
// goroutines that concurrently merge each topic into main on the
// SAME shared *Workflow. Asserts that after all four return,
// every topic commit is reachable from origin/main's tip.
// Failure mode (pre-fix): some merges race-stomp each other's
// local main update; their pushes then land orphaned (topic ref
// present, but unreachable from origin/main).
func TestMergeAcceptedTopic_FourParallelTopicsAllReachable(t *testing.T) {
	const N = 4
	bare := initBareForWorkspaceTest(t)
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(81, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Create N topic branches BEFORE any merge so they share a
	// common fork point (main's seed). Disjoint files so no
	// content conflicts when they merge into main.
	topicNames := make([]string, N)
	topicSHAs := make([]string, N)
	for i := range N {
		topicNames[i] = "topic-" + string(rune('a'+i))
		path := "out/" + topicNames[i] + ".md"
		if _, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
			Branch:      topicNames[i],
			Subject:     "seed " + topicNames[i],
			AuthorName:  "Seed",
			AuthorEmail: "seed@example.com",
			Files:       []FileWrite{{RepoRelPath: path, Content: []byte(topicNames[i])}},
		}); err != nil {
			t.Fatalf("seed %s: %v", topicNames[i], err)
		}
		sha, err := wf.git.ResolveRef(topicNames[i])
		if err != nil {
			t.Fatalf("resolve %s: %v", topicNames[i], err)
		}
		topicSHAs[i] = sha
	}

	// Re-checkout main so the worktree HEAD is on the merge
	// target before parallel merges fire (matches the production
	// entry shape from applyAcceptedMerges, where the FF path
	// skips checkout when HEAD is already on target).
	if err := wf.git.Checkout("main"); err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	// Fire N goroutines, each merging its own topic into main.
	// All on the same *Workflow → same *Clone → must serialize
	// through WithLock.
	var wg sync.WaitGroup
	errs := make([]error, N)
	wg.Add(N)
	for i := range N {
		go func(idx int) {
			defer wg.Done()
			_, mergeErr := wf.MergeAcceptedTopic(topicNames[idx], "main",
				MergeAuthor{
					TaskID:       "task-" + topicNames[idx],
					AutoOrManual: "auto",
					Citizen:      Identity{Name: "Auto", Email: "auto@example.com"},
				})
			errs[idx] = mergeErr
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("merge %s failed: %v", topicNames[i], e)
		}
	}
	if t.Failed() {
		t.FailNow()
	}

	// Read origin's truth via ls-remote — not the local clone's
	// refs/heads/main, which under the race could be ahead of
	// what actually landed on the bare. Then Fetch so the bare's
	// objects are present locally, since IsAncestor walks the
	// local object DB.
	mainTip, err := wf.git.RemoteBranchHash("main")
	if err != nil {
		t.Fatalf("ls-remote origin main: %v", err)
	}
	if mainTip == "" {
		t.Fatalf("origin/main missing on bare after %d merges", N)
	}
	if err := wf.git.Fetch(); err != nil {
		t.Fatalf("post-merge Fetch: %v", err)
	}

	missing := []string{}
	for i, sha := range topicSHAs {
		ok, err := wf.git.IsAncestor(sha, mainTip)
		if err != nil {
			t.Fatalf("IsAncestor(%s, origin/main): %v", topicNames[i], err)
		}
		if !ok {
			missing = append(missing, topicNames[i]+"="+sha[:8])
		}
	}
	if len(missing) > 0 {
		t.Fatalf("after %d parallel merges, %d topic commits not reachable from origin/main (tip=%s):\n  missing: %v\nsymptom of withlock race: parallel MergeAcceptedTopic goroutines stomped each other's local main update before push",
			N, len(missing), mainTip[:8], missing)
	}
}
