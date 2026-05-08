package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestCreateBranchAt(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	headSHA, _, _ := c.Head()
	if err := c.CreateBranchAt("topic/foo", headSHA); err != nil {
		t.Fatalf("CreateBranchAt: %v", err)
	}
	got, err := c.ResolveRef("topic/foo")
	if err != nil {
		t.Fatalf("ResolveRef new branch: %v", err)
	}
	if got != headSHA {
		t.Errorf("new branch SHA: got %s, want %s", got, headSHA)
	}
}

func TestCreateBranchAt_AlreadyExists(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	headSHA, _, _ := c.Head()
	if err := c.CreateBranchAt("topic/x", headSHA); err != nil {
		t.Fatal(err)
	}
	err := c.CreateBranchAt("topic/x", headSHA)
	if !errors.Is(err, ErrBranchExists) {
		t.Errorf("expected ErrBranchExists, got %v", err)
	}
}

func TestCreateBranchAt_BadCommit(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	err := c.CreateBranchAt("topic/x", "0000000000000000000000000000000000000000")
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

func TestDeleteBranch(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	headSHA, _, _ := c.Head()
	c.CreateBranchAt("scratch", headSHA)

	if err := c.DeleteBranch("scratch"); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}
	if _, err := c.ResolveRef("scratch"); !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound after delete, got %v", err)
	}
}

func TestDeleteBranch_NoOpOnMissing(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if err := c.DeleteBranch("nonexistent"); err != nil {
		t.Errorf("DeleteBranch missing should be no-op, got %v", err)
	}
}

func TestSetBranchTo(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("topic/x", headSHA)
	newSHA := commitOneFile(t, c, "later.txt", []byte("later"))

	if err := c.SetBranchTo("topic/x", newSHA); err != nil {
		t.Fatalf("SetBranchTo: %v", err)
	}
	got, _ := c.ResolveRef("topic/x")
	if got != newSHA {
		t.Errorf("after SetBranchTo: got %s, want %s", got, newSHA)
	}
}

func TestCheckout_SwitchesAndUpdatesWorktree(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// On main: write file A on a new branch.
	headSHA, _, _ := c.Head()
	c.CreateBranchAt("branch-a", headSHA)
	if err := c.Checkout("branch-a"); err != nil {
		t.Fatalf("Checkout branch-a: %v", err)
	}
	commitOneFile(t, c, "a.txt", []byte("from a"))

	// Switch back to main; a.txt must be gone.
	if err := c.Checkout("main"); err != nil {
		t.Fatalf("Checkout main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.workDir, "a.txt")); err == nil {
		t.Errorf("a.txt still on disk after switching to main")
	}
	_, branch, _ := c.Head()
	if branch != "main" {
		t.Errorf("expected branch=main, got %q", branch)
	}
}

func TestCheckout_PreservesUntracked(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("topic", headSHA)

	// Drop an untracked file. It should survive the checkout.
	scratch := filepath.Join(c.workDir, "scratch.notes")
	if err := os.WriteFile(scratch, []byte("important"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkout("topic"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}
	body, err := os.ReadFile(scratch)
	if err != nil {
		t.Fatalf("scratch file lost during checkout: %v", err)
	}
	if string(body) != "important" {
		t.Errorf("scratch content corrupted: %q", body)
	}
}

func TestCheckout_RefNotFound(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	err := c.Checkout("nonexistent")
	if !errors.Is(err, ErrRefNotFound) {
		t.Errorf("expected ErrRefNotFound, got %v", err)
	}
}

func TestCheckoutCommit_DetachedHEAD(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Make a commit, capture SHA, advance HEAD past it.
	firstSHA := commitOneFile(t, c, "first.txt", []byte("1"))
	commitOneFile(t, c, "second.txt", []byte("2"))

	// Checkout the older commit detached. second.txt should disappear.
	if err := c.CheckoutCommit(firstSHA); err != nil {
		t.Fatalf("CheckoutCommit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.workDir, "second.txt")); err == nil {
		t.Errorf("second.txt still present after detached checkout")
	}
	if c.State() != StateDetached {
		t.Errorf("expected StateDetached, got %s", c.State())
	}
	// first.txt should be on disk.
	if _, err := os.Stat(filepath.Join(c.workDir, "first.txt")); err != nil {
		t.Errorf("first.txt missing: %v", err)
	}
}

func TestCheckoutCommit_BadSHA(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	err := c.CheckoutCommit("0000000000000000000000000000000000000000")
	if !errors.Is(err, ErrCommitNotFound) {
		t.Errorf("expected ErrCommitNotFound, got %v", err)
	}
}

func TestResetClean_DropsUnstagedAndUntracked(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	commitOneFile(t, c, "tracked.txt", []byte("v1"))

	// Modify the tracked file + add untracked.
	os.WriteFile(filepath.Join(c.workDir, "tracked.txt"), []byte("v2"), 0o644)
	os.WriteFile(filepath.Join(c.workDir, "scratch.txt"), []byte("untracked"), 0o644)

	if err := c.ResetClean(); err != nil {
		t.Fatalf("ResetClean: %v", err)
	}
	// tracked.txt back to v1
	body, _ := os.ReadFile(filepath.Join(c.workDir, "tracked.txt"))
	if string(body) != "v1" {
		t.Errorf("tracked.txt not reset: %q", body)
	}
	// scratch.txt removed
	if _, err := os.Stat(filepath.Join(c.workDir, "scratch.txt")); err == nil {
		t.Errorf("scratch.txt should be removed")
	}
	if c.State() != StateClean {
		t.Errorf("expected StateClean after reset, got %s", c.State())
	}
}

// TestResetClean_Idempotent — daemons call ResetClean every
// iteration between tasks, even when there's nothing to clean.
// Verify that consecutive calls on an already-clean clone are
// no-ops: the worktree contents don't churn (same set of entries
// before and after) and StateClean is preserved.
func TestResetClean_Idempotent(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)
	commitOneFile(t, c, "tracked.txt", []byte("v1"))

	snapshot := func() map[string]bool {
		entries, err := os.ReadDir(c.workDir)
		if err != nil {
			t.Fatal(err)
		}
		names := map[string]bool{}
		for _, e := range entries {
			names[e.Name()] = true
		}
		return names
	}
	before := snapshot()

	for i := 0; i < 3; i++ {
		if err := c.ResetClean(); err != nil {
			t.Fatalf("call #%d: %v", i, err)
		}
		if c.State() != StateClean {
			t.Errorf("call #%d: expected StateClean, got %s", i, c.State())
		}
	}

	after := snapshot()
	if len(before) != len(after) {
		t.Errorf("entry count drifted: before=%v after=%v", before, after)
	}
	for k := range before {
		if !after[k] {
			t.Errorf("entry %q vanished after idempotent resets", k)
		}
	}
}

func TestRemoveFiles(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	os.WriteFile(filepath.Join(c.workDir, "a.txt"), []byte("a"), 0o644)
	os.WriteFile(filepath.Join(c.workDir, "b.txt"), []byte("b"), 0o644)

	if err := c.RemoveFiles([]string{"a.txt", "b.txt", "missing.txt"}); err != nil {
		t.Fatalf("RemoveFiles: %v", err)
	}
	if _, err := os.Stat(filepath.Join(c.workDir, "a.txt")); err == nil {
		t.Error("a.txt should be gone")
	}
	if _, err := os.Stat(filepath.Join(c.workDir, "b.txt")); err == nil {
		t.Error("b.txt should be gone")
	}
}
