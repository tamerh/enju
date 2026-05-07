package git

import (
	"errors"
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
