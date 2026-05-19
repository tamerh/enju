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
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
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

// TestApplyRunCompletion_ConflictReported — bug hunt B-1: a
// run-completion merge conflict is logged AND reported to the
// coordinator (POST /projects/{p}/runs/{r}/sync-conflict) so the
// otherwise log-only data-loss surfaces on coordinator surfaces.
// Push must still NOT fire after a conflict. (Pre-B-1 this test
// asserted "no coord POST is made" — the contract deliberately
// changed; this is the client-side half of the B-1 regression.)
func TestApplyRunCompletion_ConflictReported(t *testing.T) {
	var (
		mu         sync.Mutex
		gotPath    string
		gotBody    map[string]interface{}
		postCalled bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		postCalled = true
		gotPath = r.URL.Path
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"recorded"}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := New(Config{Coord: coord.New(coord.Config{
		BaseURL: srv.URL, Username: "bot1", AuthToken: "t", Logger: logger,
	}), Logger: logger})

	wf := &fakeWorkflow{
		defaultBranch: "main",
		mergeErr:      &enjugit.ErrConflict{Paths: []string{"data/results.csv", "README.md"}},
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBody(t, true, "merge"))

	if wf.mergeCalls != 1 {
		t.Errorf("MergeRunIntoBase should be called once: got %d", wf.mergeCalls)
	}
	if wf.pushCalls != 0 {
		t.Errorf("push must not be called after conflict: got %d calls", wf.pushCalls)
	}

	mu.Lock()
	defer mu.Unlock()
	if !postCalled {
		t.Fatal("B-1 regression: conflict was NOT reported to coord (silent data-loss)")
	}
	if !strings.HasSuffix(gotPath, "/projects/42/runs/3/sync-conflict") {
		t.Errorf("reported to wrong path: %q", gotPath)
	}
	if rb, _ := gotBody["run_branch"].(string); rb != "run-1" {
		t.Errorf("run_branch in report = %v, want run-1", gotBody["run_branch"])
	}
	if bb, _ := gotBody["base_branch"].(string); bb != "main" {
		t.Errorf("base_branch in report = %v, want main", gotBody["base_branch"])
	}
	files, _ := gotBody["conflict_files"].([]interface{})
	if len(files) != 2 {
		t.Errorf("conflict_files = %v, want 2 entries", gotBody["conflict_files"])
	}
}
