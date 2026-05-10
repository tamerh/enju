package gitv6

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMergeFFOrFail_FastForward(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("topic", headSHA)
	c.Checkout("topic")
	topicSHA := commitOneFile(t, c, "topic.txt", []byte("topic"))

	// Switch back to main; FF main → topic.
	c.Checkout("main")
	newTip, err := c.MergeFFOrFail("main", "topic")
	if err != nil {
		t.Fatalf("MergeFFOrFail: %v", err)
	}
	if newTip != topicSHA {
		t.Errorf("FF tip: got %s, want %s", newTip, topicSHA)
	}
}

// TestMergeFFOrFail_TargetOnlyOnOrigin pins the bot-clone fix:
// when the target branch exists as origin/<name> in the merge
// clone but NOT as a local refs/heads/<name>, MergeFFOrFail must
// auto-plant the local ref from origin tracking and proceed.
//
// Repro flow:
//  1. Bare has main + run-branch "smoke-1" (created upstream).
//  2. Bot's per-bot clone fetches → has refs/remotes/origin/smoke-1
//     but no refs/heads/smoke-1 (never explicitly tracked locally).
//  3. Bot creates topic, commits, pushes.
//  4. Topic accepted → MergeAcceptedTopic asks to merge topic
//     into smoke-1. Without the fix, MergeFFOrFail returns
//     ErrRefNotFound mapped to "upstream branch not found on
//     origin" — even though origin DOES have it.
//  5. With the fix, the local smoke-1 is planted from
//     origin/smoke-1 automatically and the merge proceeds.
func TestMergeFFOrFail_TargetOnlyOnOrigin(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Step 1: prepare the run branch on the bare. Use the same
	// clone to push smoke-1, then DELETE the local ref so the
	// post-state matches a fresh clone that only sees origin/smoke-1.
	rootSHA, _, _ := c.Head()
	c.CreateBranchAt("smoke-1", rootSHA)
	c.Checkout("smoke-1")
	smoke1SHA := commitOneFile(t, c, "run-marker.txt", []byte("run"))
	if err := c.Push("smoke-1"); err != nil {
		t.Fatalf("seed push smoke-1: %v", err)
	}

	// Now produce the "bot's clone" pre-state: fresh clone from
	// the bare. After fresh clone + Fetch, smoke-1 lives at
	// refs/remotes/origin/smoke-1 only — no local ref.
	bot := freshClone(t, bare)
	if err := bot.Fetch(); err != nil {
		t.Fatalf("bot fetch: %v", err)
	}
	if _, err := bot.resolveLocalRef("smoke-1"); err == nil {
		t.Fatal("precondition failure: bot clone unexpectedly has local smoke-1; reproducer is invalid")
	}

	// Step 2: bot creates topic from smoke-1 and commits.
	bot.CreateBranchAt("topic-a", smoke1SHA)
	bot.Checkout("topic-a")
	topicSHA := commitOneFile(t, bot, "topic.txt", []byte("topic"))

	// Step 3: the test — merge topic into smoke-1. Pre-fix this
	// returns ErrRefNotFound; post-fix it auto-plants local
	// smoke-1 from origin/smoke-1 and FFs cleanly.
	newTip, err := bot.MergeFFOrFail("smoke-1", "topic-a")
	if err != nil {
		t.Fatalf("MergeFFOrFail with target on origin only: %v", err)
	}
	if newTip != topicSHA {
		t.Errorf("FF tip: got %s, want %s", newTip, topicSHA)
	}

	// Verify: refs/heads/smoke-1 now exists in the bot clone.
	if _, err := bot.resolveLocalRef("smoke-1"); err != nil {
		t.Errorf("expected local smoke-1 ref planted post-merge, got %v", err)
	}
}

func TestMergeFFOrFail_NotFastForward(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Branch from same root; both diverge.
	rootSHA, _, _ := c.Head()
	c.CreateBranchAt("topic-a", rootSHA)
	c.Checkout("topic-a")
	commitOneFile(t, c, "a.txt", []byte("a"))

	c.Checkout("main")
	commitOneFile(t, c, "main.txt", []byte("m"))

	// main and topic-a have diverged; FF impossible.
	_, err := c.MergeFFOrFail("main", "topic-a")
	if !errors.Is(err, ErrPushNonFF) {
		t.Errorf("expected ErrPushNonFF, got %v", err)
	}
}

func TestMergeWithCommit_DisjointFiles(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	rootSHA, _, _ := c.Head()
	c.CreateBranchAt("topic", rootSHA)
	c.Checkout("topic")
	commitOneFile(t, c, "topic-only.txt", []byte("t"))

	c.Checkout("main")
	commitOneFile(t, c, "main-only.txt", []byte("m"))

	// Both touched DIFFERENT files → no conflict, merge commit.
	newTip, err := c.MergeWithCommit("main", "topic", "auto-merge", "Test", "test@x.com")
	if err != nil {
		t.Fatalf("MergeWithCommit: %v", err)
	}
	if !isHexSHA(newTip) {
		t.Errorf("expected new tip SHA, got %q", newTip)
	}
}

func TestMergeWithCommit_Conflict(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	rootSHA, _, _ := c.Head()
	c.CreateBranchAt("topic", rootSHA)
	c.Checkout("topic")
	commitOneFile(t, c, "shared.txt", []byte("topic-version"))

	c.Checkout("main")
	commitOneFile(t, c, "shared.txt", []byte("main-version"))

	// Same file, conflicting content.
	_, err := c.MergeWithCommit("main", "topic", "auto-merge", "Test", "test@x.com")
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("expected ErrMergeConflict, got %v", err)
	}
	// The error should carry conflict paths via ErrConflict.
	var conflict *ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected ErrConflict in chain, got %v", err)
	}
	if len(conflict.Paths) == 0 {
		t.Errorf("expected at least one conflict path")
	}
	if !strings.Contains(strings.Join(conflict.Paths, ","), "shared.txt") {
		t.Errorf("expected shared.txt in conflict paths: %v", conflict.Paths)
	}
}

func TestWithLock_HoldsAcrossOps(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Just verify the API works. Multi-op semantics are exercised
	// by enjugit's tests against real workflows.
	called := false
	err := c.WithLock(func(g Ops) error {
		called = true
		// Verify nested Head() (a read, no lock needed) works.
		_, _, err := g.Head()
		return err
	})
	if err != nil {
		t.Fatalf("WithLock: %v", err)
	}
	if !called {
		t.Error("WithLock didn't invoke fn")
	}
}

func TestWithLock_Reentrant(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Nested WithLock must not deadlock.
	err := c.WithLock(func(g Ops) error {
		return g.WithLock(func(g2 Ops) error {
			_, _, err := g2.Head()
			return err
		})
	})
	if err != nil {
		t.Errorf("reentrant WithLock: %v", err)
	}
}

func TestCompareToRemote_InSync(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	cmp, err := c.CompareToRemote([]string{"main"})
	if err != nil {
		t.Fatalf("CompareToRemote: %v", err)
	}
	if len(cmp.Branches) != 1 {
		t.Fatalf("expected 1 branch, got %d", len(cmp.Branches))
	}
	if cmp.Branches[0].State != RemoteInSync {
		t.Errorf("expected InSync, got %s", cmp.Branches[0].State)
	}
}

func TestCompareToRemote_Ahead(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	commitOneFile(t, c, "x.txt", []byte("x"))
	cmp, err := c.CompareToRemote([]string{"main"})
	if err != nil {
		t.Fatalf("CompareToRemote: %v", err)
	}
	if cmp.Branches[0].State != RemoteAhead {
		t.Errorf("expected Ahead, got %s", cmp.Branches[0].State)
	}
}

// TestMergeWithCommit_DirtyWorktreeDoesNotBlock pins the Phase 1
// architectural property: non-FF merges work even when the
// worktree has untracked stragglers at paths the merge would
// touch. Pre-Phase-1 implementation shelled out to
// `git checkout + git merge --no-ff`, which refused with
// "untracked working tree files would be overwritten by merge."
// The plumbing path uses `git merge-tree --write-tree` and never
// reads from or writes to the worktree.
//
// Repros the load-test scenario where parallel sibling tasks
// leave files in a shared worktree (pre-Phase-2.3) or where any
// other process has unrelated untracked state on disk.
func TestMergeWithCommit_DirtyWorktreeDoesNotBlock(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Two divergent branches: main has main-only.txt, topic has
	// data/raw_b.txt. Merge needs a non-FF commit.
	rootSHA, _, _ := c.Head()
	c.CreateBranchAt("topic", rootSHA)
	c.Checkout("topic")
	commitOneFile(t, c, "data/raw_b.txt", []byte("topic-version"))

	c.Checkout("main")
	commitOneFile(t, c, "main-only.txt", []byte("main"))

	// Plant an UNTRACKED file in the worktree at the same path
	// the merge would create. With the pre-Phase-1 shellout this
	// causes "would be overwritten by merge" and the merge fails.
	// With plumbing merge: never touched, never noticed.
	dirty := filepath.Join(c.workDir, "data")
	if err := os.MkdirAll(dirty, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dirty, "raw_b.txt"),
		[]byte("untracked-stranger"), 0o644); err != nil {
		t.Fatalf("write untracked: %v", err)
	}

	newTip, err := c.MergeWithCommit("main", "topic",
		"merge topic into main", "Test", "test@x.com")
	if err != nil {
		t.Fatalf("MergeWithCommit (worktree dirty): %v", err)
	}
	if !isHexSHA(newTip) {
		t.Errorf("expected merge commit SHA, got %q", newTip)
	}

	// The untracked stranger should still be on disk afterwards
	// — the merge never touched the worktree.
	body, rerr := os.ReadFile(filepath.Join(dirty, "raw_b.txt"))
	if rerr != nil {
		t.Errorf("untracked file disappeared after merge: %v", rerr)
	} else if string(body) != "untracked-stranger" {
		t.Errorf("untracked file got clobbered: got %q, want untracked-stranger", body)
	}
}
