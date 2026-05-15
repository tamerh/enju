package enjugit

// Tests for Workflow.MergeRunIntoBase — the run-completion merge
// that advances base_branch to include the finished run's commits.

import (
	"errors"
	"testing"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

var systemAuthor = MergeAuthor{
	Citizen:      Identity{Name: "enju", Email: "enju@local"},
	AutoOrManual: "auto",
}

// notYetMerged configures fakeOps to report that run branch is NOT
// yet an ancestor of base branch, so MergeRunIntoBase proceeds past
// the idempotency guard to the actual merge logic.
func notYetMerged(fake *fakeOps) {
	fake.ancestorReturnSet = true
	fake.ancestorReturn = false
}

// TestMergeRunIntoBase_FastForward — run branch is a linear
// descendant of base_branch; MergeFFOrFail succeeds; result is
// marked fast-forwarded.
func TestMergeRunIntoBase_FastForward(t *testing.T) {
	wf, fake := makeWorkflow(t)
	notYetMerged(fake)

	res, err := wf.MergeRunIntoBase("run-1", "main", systemAuthor)
	if err != nil {
		t.Fatalf("MergeRunIntoBase: %v", err)
	}
	if !res.FastForwarded {
		t.Error("expected FastForwarded=true on clean FF")
	}
	if res.NewTip != "ffsha" {
		t.Errorf("NewTip: got %q, want %q", res.NewTip, "ffsha")
	}
	if fake.callCount("MergeWithCommit") != 0 {
		t.Error("MergeWithCommit should not be called on a clean FF")
	}
}

// TestMergeRunIntoBase_NonFFMergeCommit — base_branch has advanced
// since the run forked (parallel run landed); MergeFFOrFail fails
// with ErrPushNonFF, falls back to a merge commit.
func TestMergeRunIntoBase_NonFFMergeCommit(t *testing.T) {
	wf, fake := makeWorkflow(t)
	notYetMerged(fake)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)

	res, err := wf.MergeRunIntoBase("run-1", "main", systemAuthor)
	if err != nil {
		t.Fatalf("MergeRunIntoBase non-FF: %v", err)
	}
	if res.FastForwarded {
		t.Error("expected FastForwarded=false on merge-commit path")
	}
	if res.NewTip != "mergesha" {
		t.Errorf("NewTip: got %q, want %q", res.NewTip, "mergesha")
	}
	if fake.callCount("MergeWithCommit") != 1 {
		t.Errorf("expected 1 MergeWithCommit call, got %d", fake.callCount("MergeWithCommit"))
	}
}

// TestMergeRunIntoBase_ConflictTranslated — MergeWithCommit returns
// a git.ErrConflict; the verb wraps it into *ErrConflict so the
// caller can detect and report it.
func TestMergeRunIntoBase_ConflictTranslated(t *testing.T) {
	wf, fake := makeWorkflow(t)
	notYetMerged(fake)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)
	fake.inject("MergeWithCommit", &git.ErrConflict{Paths: []string{"data/results.csv"}})

	_, err := wf.MergeRunIntoBase("run-1", "main", systemAuthor)
	if err == nil {
		t.Fatal("expected error from conflict, got nil")
	}
	var conflict *ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *ErrConflict, got %T: %v", err, err)
	}
	if !errors.Is(err, ErrMergeConflict) {
		t.Error("errors.Is(err, ErrMergeConflict) should be true")
	}
}

// TestMergeRunIntoBase_Idempotent — run branch is already an
// ancestor of base_branch (fatclient restarted after run completed
// but before the merge was persisted). Must be a no-op.
func TestMergeRunIntoBase_Idempotent(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Default fakeOps.IsAncestor returns true → already merged.

	res, err := wf.MergeRunIntoBase("run-1", "main", systemAuthor)
	if err != nil {
		t.Fatalf("MergeRunIntoBase idempotent: %v", err)
	}
	if !res.FastForwarded {
		t.Error("expected FastForwarded=true for already-merged no-op")
	}
	if fake.callCount("MergeFFOrFail") != 0 {
		t.Error("MergeFFOrFail should not be called when already merged")
	}
	if fake.callCount("MergeWithCommit") != 0 {
		t.Error("MergeWithCommit should not be called when already merged")
	}
}

// TestMergeRunIntoBase_RequiresBothBranches — empty runBranch or
// baseBranch is a programming error; returns a clear error without
// touching git.
func TestMergeRunIntoBase_RequiresBothBranches(t *testing.T) {
	wf, _ := makeWorkflow(t)

	if _, err := wf.MergeRunIntoBase("", "main", systemAuthor); err == nil {
		t.Error("expected error for empty runBranch")
	}
	if _, err := wf.MergeRunIntoBase("run-1", "", systemAuthor); err == nil {
		t.Error("expected error for empty baseBranch")
	}
}
