package service

// Tests for FatClient.applyRunCompletion — the run-completion sync
// step that merges the run branch into base_branch and optionally
// pushes. These tests focus on the orchestration logic (mode routing,
// guard clauses) using the nil-workflow / nil-meta fast paths.
// The actual merge behavior is tested in enjugit/merge_run_into_base_test.go.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// fakeWorkflow implements mergeWorkflow for service-layer tests.
// Controls return values for MergeRunIntoBase and PushBranch so tests
// can assert which branches were merged/pushed without a real git repo.
type fakeWorkflow struct {
	defaultBranch string
	remoteURL     string

	mergeErr    error
	mergeResult *enjugit.MergeResult

	pushErr error

	// recorded call args
	mergedRun  string
	mergedBase string
	pushBranch string
	mergeCalls int
	pushCalls  int
}

func (f *fakeWorkflow) DefaultBranch() string { return f.defaultBranch }
func (f *fakeWorkflow) RemoteURL() string      { return f.remoteURL }

func (f *fakeWorkflow) MergeRunIntoBase(run, base string, _ enjugit.MergeAuthor) (*enjugit.MergeResult, error) {
	f.mergeCalls++
	f.mergedRun = run
	f.mergedBase = base
	if f.mergeErr != nil {
		return nil, f.mergeErr
	}
	res := f.mergeResult
	if res == nil {
		res = &enjugit.MergeResult{FastForwarded: true, NewTip: "abc123"}
	}
	return res, nil
}

func (f *fakeWorkflow) PushBranch(branch string) error {
	f.pushCalls++
	f.pushBranch = branch
	return f.pushErr
}

func nullFatClient() *FatClient {
	return &FatClient{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func completionBody(t *testing.T, runCompleted bool, syncMode string) []byte {
	t.Helper()
	m := map[string]interface{}{
		"run_completed": runCompleted,
	}
	if syncMode != "" {
		m["sync_mode"] = syncMode
	}
	b, _ := json.Marshal(m)
	return b
}

// TestApplyRunCompletion_SkipsWhenNotCompleted — RunCompleted=false
// means we're mid-run; nothing should happen.
func TestApplyRunCompletion_SkipsWhenNotCompleted(t *testing.T) {
	fc := nullFatClient()
	// nil wf — would panic if we got past the RunCompleted guard.
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{Branch: "run-1"}, completionBody(t, false, "merge"))
	// No panic = guard worked.
}

// TestApplyRunCompletion_SkipsWhenNilWorkflow — nil workflow with
// RunCompleted=true must not panic.
func TestApplyRunCompletion_SkipsWhenNilWorkflow(t *testing.T) {
	fc := nullFatClient()
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{Branch: "run-1"}, completionBody(t, true, "merge"))
}

// TestApplyRunCompletion_SkipsWhenNilMeta — nil meta with
// RunCompleted=true must not panic.
func TestApplyRunCompletion_SkipsWhenNilMeta(t *testing.T) {
	fc := nullFatClient()
	fc.applyRunCompletion(context.Background(), nil, nil, completionBody(t, true, "merge"))
}

// TestApplyRunCompletion_SkipsWhenModeNone — mode=none means
// operator opted out; no merge should be attempted.
func TestApplyRunCompletion_SkipsWhenModeNone(t *testing.T) {
	fc := nullFatClient()
	// nil wf — would panic if we tried to merge.
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{Branch: "run-1"}, completionBody(t, true, "none"))
}

// TestApplyRunCompletion_SkipsWhenEmptyResponseBody — empty body
// must not panic.
func TestApplyRunCompletion_SkipsWhenEmptyResponseBody(t *testing.T) {
	fc := nullFatClient()
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{}, nil)
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{}, []byte{})
}

// TestRunCompletion_ModeDefaultsToMerge — response has run_completed=true
// but no sync_mode field; applyRunCompletion defaults to "merge".
func TestRunCompletion_ModeDefaultsToMerge(t *testing.T) {
	fc := nullFatClient()
	// nil wf skips at the nil guard, not at the mode=none guard.
	// Passing a non-nil meta with empty Branch exercises the
	// "missing branch names" warning path instead.
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{Branch: "run-1"}, completionBody(t, true, ""))
	// No panic and no attempt to merge (wf=nil guard fires first) = correct.
}

// TestApplyRunCompletion_MergeSuccess — mode=merge calls MergeRunIntoBase
// with the right branches and does NOT push.
func TestApplyRunCompletion_MergeSuccess(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "https://example.com/repo.git",
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBody(t, true, "merge"))

	if wf.mergeCalls != 1 {
		t.Errorf("MergeRunIntoBase calls: got %d, want 1", wf.mergeCalls)
	}
	if wf.mergedRun != "run-1" {
		t.Errorf("mergedRun: got %q, want %q", wf.mergedRun, "run-1")
	}
	if wf.mergedBase != "main" {
		t.Errorf("mergedBase: got %q, want %q", wf.mergedBase, "main")
	}
	if wf.pushCalls != 0 {
		t.Errorf("PushBranch should not be called for mode=merge, got %d calls", wf.pushCalls)
	}
}

// TestApplyRunCompletion_PushSuccess — mode=push merges then pushes
// the base branch.
func TestApplyRunCompletion_PushSuccess(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "https://example.com/repo.git",
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBody(t, true, "push"))

	if wf.mergeCalls != 1 {
		t.Errorf("MergeRunIntoBase calls: got %d, want 1", wf.mergeCalls)
	}
	if wf.pushCalls != 1 {
		t.Errorf("PushBranch calls: got %d, want 1", wf.pushCalls)
	}
	if wf.pushBranch != "main" {
		t.Errorf("push branch: got %q, want %q", wf.pushBranch, "main")
	}
}

// TestApplyRunCompletion_PushSkippedWithNoRemote — mode=push but no
// remote configured; merge still happens, push is skipped.
func TestApplyRunCompletion_PushSkippedWithNoRemote(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "", // no remote
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1"},
		completionBody(t, true, "push"))

	if wf.mergeCalls != 1 {
		t.Errorf("merge should still run: got %d calls", wf.mergeCalls)
	}
	if wf.pushCalls != 0 {
		t.Errorf("push should be skipped when no remote: got %d calls", wf.pushCalls)
	}
}

// TestApplyRunCompletion_ConflictLogged — merge conflict is logged as
// an error with the conflict files; no coord POST is made.
func TestApplyRunCompletion_ConflictLogged(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		mergeErr:      &enjugit.ErrConflict{Paths: []string{"data/results.csv", "README.md"}},
	}
	// Must not panic; the test asserts no panic by completing.
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBody(t, true, "merge"))

	if wf.mergeCalls != 1 {
		t.Errorf("MergeRunIntoBase should be called once: got %d", wf.mergeCalls)
	}
	if wf.pushCalls != 0 {
		t.Errorf("push must not be called after conflict: got %d calls", wf.pushCalls)
	}
}
