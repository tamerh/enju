package enjugit

// Reproducing test for the per-task push non-FF failure exposed by
// `enju go --parallel N` (see parallel-push-race.bug.md at repo root).
//
// Mechanic (deterministic, no goroutines needed): MergeAcceptedTopic
// fetches before merging, but the fetch only advances the remote-
// tracking ref (origin/<target>); the merge step merges the TOPIC into
// the LOCAL <target> and never incorporates origin/<target>. So when
// origin has advanced since this clone last synced its local <target>,
// the post-merge push is rejected non-fast-forward — and there is no
// fetch-merge-retry, so it goes straight to the terminal merge_failed
// path.
//
// `--parallel` is only the TRIGGER: a sibling task pushes between this
// task's fetch and push, moving origin. We reproduce that by having a
// SECOND clone of the same bare advance origin/main behind the first
// clone's back — exactly the state a concurrent sibling push leaves.
//
// This test asserts the DESIRED (post-fix) behavior: the merge should
// catch up to origin and the push should land. It is RED today (the
// push is rejected) and turns GREEN once MergeAcceptedTopic retries the
// push with a fetch+catch-up on non-FF (fix #1 in the bug report).

import (
	"testing"
)

func TestMergeAcceptedTopic_PushNonFF_CatchesUpAndLands(t *testing.T) {
	bare := initBareForWorkspaceTest(t) // origin/main = seed

	// Two independent clones of the SAME bare. Separate workspaces →
	// separate .git → refs are NOT shared; they only meet at origin.
	wsA, _ := newWorkspaceForIDs(t, 81)
	wfA, err := wsA.ForProject(81, bare)
	if err != nil {
		t.Fatalf("ForProject A: %v", err)
	}
	wsB, _ := newWorkspaceForIDs(t, 82)
	wfB, err := wsB.ForProject(82, bare)
	if err != nil {
		t.Fatalf("ForProject B: %v", err)
	}

	// Clone A forks a topic from the original main (seed) and commits.
	if _, err := wfA.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "topic-a",
		Subject:     "topic a work",
		AuthorName:  "A",
		AuthorEmail: "a@example.com",
		Files:       []FileWrite{{RepoRelPath: "out/a.md", Content: []byte("a")}},
	}); err != nil {
		t.Fatalf("commit topic-a: %v", err)
	}
	topicASHA, err := wfA.git.ResolveRef("topic-a")
	if err != nil {
		t.Fatalf("resolve topic-a: %v", err)
	}

	// Clone B advances origin/main behind A's back — the "sibling task
	// pushed first" state. CommitArbitraryFiles pushes when origin is set.
	if _, err := wfB.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "main",
		Subject:     "sibling advanced main",
		AuthorName:  "B",
		AuthorEmail: "b@example.com",
		Files:       []FileWrite{{RepoRelPath: "out/y.md", Content: []byte("y")}},
	}); err != nil {
		t.Fatalf("commit+push main from B: %v", err)
	}
	siblingMainSHA, err := wfB.git.RemoteBranchHash("main")
	if err != nil || siblingMainSHA == "" {
		t.Fatalf("read advanced origin/main: sha=%q err=%v", siblingMainSHA, err)
	}

	// Entry shape matches production (applyAcceptedMerges): HEAD on the
	// merge target before the merge.
	if err := wfA.git.Checkout("main"); err != nil {
		t.Fatalf("checkout main on A: %v", err)
	}

	// The operation under test. A's local main is still at seed; origin
	// is at seed+Y. A's fetch will see origin/main=seed+Y but the merge
	// merges topic-a into local main (seed) and the push is non-FF.
	_, mergeErr := wfA.MergeAcceptedTopic("topic-a", "main", MergeAuthor{
		TaskID:       "2:9:topic-a",
		AutoOrManual: "auto",
		Citizen:      Identity{Name: "Auto", Email: "auto@example.com"},
	})
	if mergeErr != nil {
		t.Fatalf("REPRODUCED BUG: MergeAcceptedTopic rejected non-FF instead of "+
			"catching up to origin and re-pushing.\n  err: %v\n"+
			"  (fix #1: on ErrPushNonFF, fetch + merge origin/main into local "+
			"main, then re-push, bounded.)", mergeErr)
	}

	// Post-fix expectations: origin/main contains BOTH the sibling's
	// commit and topic-a's work, and reports them reachable.
	finalMain, err := wfA.git.RemoteBranchHash("main")
	if err != nil || finalMain == "" {
		t.Fatalf("read final origin/main: sha=%q err=%v", finalMain, err)
	}
	if err := wfA.git.Fetch(); err != nil {
		t.Fatalf("post-merge fetch: %v", err)
	}
	for _, want := range []struct {
		label string
		sha   string
	}{{"topic-a", topicASHA}, {"sibling main", siblingMainSHA}} {
		ok, aerr := wfA.git.IsAncestor(want.sha, finalMain)
		if aerr != nil {
			t.Fatalf("IsAncestor(%s): %v", want.label, aerr)
		}
		if !ok {
			t.Errorf("after catch-up merge, %s (%s) is not reachable from origin/main (%s)",
				want.label, want.sha[:8], finalMain[:8])
		}
	}
}
