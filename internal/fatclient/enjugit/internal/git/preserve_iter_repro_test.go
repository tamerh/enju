package git

// Reproduces the v17 build-3 develop_scaffold bug observed
// 2026-05-09: opus committed scaffold on iter-1 (attempt 1), got
// request_changes, committed iter-1 attempt 2 (with the warning
// "preserved files collide with new branch tree; left in preserve
// dir"), got request_changes again, then on attempt 3 found
// `src/` entirely missing on disk → workspace_prep submitted
// empty → coordinator rejected with "required writes_artifacts
// missing on disk."
//
// Key signals the live bug left:
//   - preserve dir on disk contains the src/ subtree
//   - workDir lacks src/ entirely
//   - clone was on parent `build-3`, NOT the iter branch
//   - all 7 declared writes_artifacts missing
//
// This test pins down whether the published Checkout/preserve
// dance actually restores branch content into workDir on the
// next attempt, given the exact pre-state we observed in the
// wild. Skip with go test -run TestPreserveIterRepro -short=false.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestPreserveIterRepro_BranchContentRestoredAfterRequestChanges
// simulates the full bot-iteration cycle:
//
//	attempt 1: bot writes src/{a,b,c}.txt, commits to iter-1
//	review:    rejected
//	attempt 2: bot edits src/{a,b}.txt (still on iter-1, fresh
//	           commit). For the bug we observed, attempt 2's
//	           submit logged "preserved files collide" — meaning
//	           the daemon's workspace prep ran into colliding
//	           paths between the fresh tree and the iter branch
//	           and left them in the preserve dir.
//	review:    rejected
//	attempt 3: workspace prep re-runs. Expectation: workDir/src
//	           contains the iter-1 attempt-2 commit's content
//	           (a.txt v2, b.txt v2, c.txt v1). Live bug: workDir
//	           has no src/ at all.
func TestPreserveIterRepro_BranchContentRestoredAfterRequestChanges(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// --- attempt 1: write three files, commit to iter branch ---
	iterBranch := "3-build/develop_scaffold/iter-1"
	headSHA, _, _ := c.Head()
	if err := c.CreateBranchAt(iterBranch, headSHA); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkout(iterBranch); err != nil {
		t.Fatalf("checkout iter: %v", err)
	}

	writeIter := func(label string) {
		for _, name := range []string{"a", "b", "c"} {
			full := filepath.Join(c.workDir, "src", name+".txt")
			if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(full, []byte(label+"-"+name+"\n"), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	writeIter("v1")

	if _, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "src/a.txt", Content: []byte("v1-a\n")},
			{RepoRelPath: "src/b.txt", Content: []byte("v1-b\n")},
			{RepoRelPath: "src/c.txt", Content: []byte("v1-c\n")},
		},
		Message: "iter-1 attempt 1: scaffold v1",
	}); err != nil {
		t.Fatalf("commit attempt 1: %v", err)
	}

	// --- request_changes: daemon prepares for attempt 2 ---
	// The daemon's behavior we want to mimic: it calls Checkout
	// against the iter branch (re-asserting workDir matches the
	// branch HEAD before the next handler invocation).
	if err := c.Checkout(iterBranch); err != nil {
		t.Fatalf("checkout for attempt 2: %v", err)
	}

	// Verify attempt 1 content is on disk before attempt 2 runs.
	for _, want := range []struct{ rel, body string }{
		{"src/a.txt", "v1-a\n"},
		{"src/b.txt", "v1-b\n"},
		{"src/c.txt", "v1-c\n"},
	} {
		got, err := os.ReadFile(filepath.Join(c.workDir, want.rel))
		if err != nil {
			t.Fatalf("pre-attempt-2 read %s: %v", want.rel, err)
		}
		if string(got) != want.body {
			t.Errorf("pre-attempt-2 %s = %q, want %q", want.rel, got, want.body)
		}
	}

	// --- attempt 2: bot edits a.txt and b.txt, leaves c.txt ---
	writeIter("v2") // overwrites a/b/c on disk
	if _, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: "src/a.txt", Content: []byte("v2-a\n")},
			{RepoRelPath: "src/b.txt", Content: []byte("v2-b\n")},
			// c.txt unchanged — explicitly NOT in the commit
		},
		Message: "iter-1 attempt 2: scaffold v2",
	}); err != nil {
		t.Fatalf("commit attempt 2: %v", err)
	}

	// --- request_changes again: daemon prepares for attempt 3 ---
	// THIS is where the bug surfaced. workspace_prep re-checks
	// out the iter branch. If preserve+restore is buggy, the
	// workDir ends up empty.
	if err := c.Checkout(iterBranch); err != nil {
		t.Fatalf("checkout for attempt 3: %v", err)
	}

	// Expected: workDir/src/{a,b,c}.txt all present with the
	// attempt-2 commit's content.
	for _, want := range []struct{ rel, body string }{
		{"src/a.txt", "v2-a\n"},
		{"src/b.txt", "v2-b\n"},
		{"src/c.txt", "v1-c\n"},
	} {
		got, err := os.ReadFile(filepath.Join(c.workDir, want.rel))
		if err != nil {
			t.Errorf("attempt-3 missing %s: %v (live bug pattern)", want.rel, err)
			continue
		}
		if string(got) != want.body {
			t.Errorf("attempt-3 %s = %q, want %q", want.rel, got, want.body)
		}
	}

	// Preserve dir should not be left behind blocking attempt 3.
	if _, err := os.Stat(c.workDir + PreserveDirSuffix); err == nil {
		t.Errorf("preserve dir still present after attempt-3 prep — orphans pile up across iters")
	}
}
