package service

// Tests for FatClient.applyRunCompletion — the run-completion sync
// step that publishes the run's declared output set onto the base
// branch and, in push mode, shares { base, run branch } (+ topic
// branches only when opted in). These focus on the orchestration
// logic (mode routing, guard clauses, request shaping). The actual
// git publish behavior is tested in
// enjugit/publish_run_artifacts_test.go.

import (
	"context"
	"encoding/json"
	"errors"
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
// Records the PublishRunArtifacts request so tests can assert what
// applyRunCompletion asked for without a real git repo.
type fakeWorkflow struct {
	defaultBranch string
	remoteURL     string

	publishErr    error
	publishResult *enjugit.PublishRunArtifactsResult

	// recorded call args
	gotReq       enjugit.PublishRunArtifactsRequest
	publishCalls int
}

func (f *fakeWorkflow) DefaultBranch() string { return f.defaultBranch }
func (f *fakeWorkflow) RemoteURL() string     { return f.remoteURL }

func (f *fakeWorkflow) PublishRunArtifacts(req enjugit.PublishRunArtifactsRequest) (*enjugit.PublishRunArtifactsResult, error) {
	f.publishCalls++
	f.gotReq = req
	if f.publishErr != nil {
		return nil, f.publishErr
	}
	res := f.publishResult
	if res == nil {
		res = &enjugit.PublishRunArtifactsResult{CommitSHA: "abc123"}
	}
	return res, nil
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

// completionBodyFull builds a completion response with the declared
// publish set + the topic-push opt-in, the shape the coordinator
// sends on a real run completion.
func completionBodyFull(t *testing.T, syncMode string, paths []string, pushTopics bool) []byte {
	t.Helper()
	m := map[string]interface{}{
		"run_completed": true,
		"sync_mode":     syncMode,
		"publish_paths": paths,
		"push_topics":   pushTopics,
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
// operator opted out; no publish should be attempted.
func TestApplyRunCompletion_SkipsWhenModeNone(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{defaultBranch: "main"}
	fc.applyRunCompletion(context.Background(), wf, &TaskMeta{Branch: "run-1"}, completionBody(t, true, "none"))
	if wf.publishCalls != 0 {
		t.Errorf("mode=none must not publish: got %d calls", wf.publishCalls)
	}
}

// TestApplyRunCompletion_SkipsWhenEmptyResponseBody — empty body
// must not panic.
func TestApplyRunCompletion_SkipsWhenEmptyResponseBody(t *testing.T) {
	fc := nullFatClient()
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{}, nil)
	fc.applyRunCompletion(context.Background(), nil, &TaskMeta{}, []byte{})
}

// TestRunCompletion_ModeDefaultsToMerge — response has
// run_completed=true but no sync_mode; defaults to "merge" (publish,
// no push).
func TestRunCompletion_ModeDefaultsToMerge(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{defaultBranch: "main"}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1"}, completionBody(t, true, ""))
	if wf.publishCalls != 1 {
		t.Fatalf("default mode must publish once: got %d", wf.publishCalls)
	}
	if wf.gotReq.Push {
		t.Errorf("default (merge) mode must not push")
	}
}

// TestApplyRunCompletion_MergePublishesNoPush — mode=merge publishes
// the declared set onto base with the right branches and Push=false.
func TestApplyRunCompletion_MergePublishesNoPush(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "https://example.com/repo.git",
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBodyFull(t, "merge", []string{"results/out.csv"}, false))

	if wf.publishCalls != 1 {
		t.Fatalf("PublishRunArtifacts calls: got %d, want 1", wf.publishCalls)
	}
	if wf.gotReq.RunBranch != "run-1" || wf.gotReq.BaseBranch != "main" {
		t.Errorf("branches: got run=%q base=%q", wf.gotReq.RunBranch, wf.gotReq.BaseBranch)
	}
	if len(wf.gotReq.Paths) != 1 || wf.gotReq.Paths[0] != "results/out.csv" {
		t.Errorf("publish paths not passed through: %v", wf.gotReq.Paths)
	}
	if wf.gotReq.Push {
		t.Errorf("mode=merge must not push (Push=true)")
	}
}

// TestApplyRunCompletion_PushSharesBaseAndRunBranch — mode=push asks
// for the push of { base, run branch }; push_topics defaults off.
func TestApplyRunCompletion_PushSharesBaseAndRunBranch(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "https://example.com/repo.git",
		publishResult: &enjugit.PublishRunArtifactsResult{
			CommitSHA: "deadbeef",
			Pushed:    []string{"main", "run-1"},
		},
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBodyFull(t, "push", []string{"results/out.csv"}, false))

	if wf.publishCalls != 1 {
		t.Fatalf("publish calls: got %d, want 1", wf.publishCalls)
	}
	if !wf.gotReq.Push {
		t.Errorf("mode=push must set Push=true")
	}
	if wf.gotReq.PushTopics {
		t.Errorf("push_topics must default off")
	}
}

// TestApplyRunCompletion_PushTopicsPassedThrough — sync.push_topics
// surfaces on the request so the enjugit layer also pushes topics.
func TestApplyRunCompletion_PushTopicsPassedThrough(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "https://example.com/repo.git",
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBodyFull(t, "push", []string{"out.txt"}, true))

	if !wf.gotReq.Push || !wf.gotReq.PushTopics {
		t.Errorf("push_topics opt-in not threaded: Push=%v PushTopics=%v",
			wf.gotReq.Push, wf.gotReq.PushTopics)
	}
}

// TestApplyRunCompletion_PushNoRemotePublishesLocal — mode=push but
// no remote: still publishes locally, with Push downgraded to false
// (base gets the artifacts-only update; operator pushes by hand).
func TestApplyRunCompletion_PushNoRemotePublishesLocal(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		remoteURL:     "", // no remote
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1"},
		completionBodyFull(t, "push", []string{"out.txt"}, false))

	if wf.publishCalls != 1 {
		t.Fatalf("publish should still run locally: got %d calls", wf.publishCalls)
	}
	if wf.gotReq.Push {
		t.Errorf("push must be downgraded to false when no remote configured")
	}
}

// TestApplyRunCompletion_PublishErrorNonFatal — a hard publish
// failure is logged, not panicked or returned. (applyRunCompletion
// has no error return; the contract is best-effort.)
func TestApplyRunCompletion_PublishErrorNonFatal(t *testing.T) {
	fc := nullFatClient()
	wf := &fakeWorkflow{
		defaultBranch: "main",
		publishErr:    errors.New("read declared artifact: object missing"),
	}
	fc.applyRunCompletion(context.Background(), wf,
		&TaskMeta{Branch: "run-1", ProjectID: 42, RunSeq: 3},
		completionBodyFull(t, "merge", []string{"out.txt"}, false))
	if wf.publishCalls != 1 {
		t.Errorf("publish should be attempted once: got %d", wf.publishCalls)
	}
	// No panic / no second attempt = correct best-effort handling.
}

// TestReportRunSyncConflict_PostsSignal exercises the retained
// run_sync_conflict client directly. applyRunCompletion no longer
// auto-fires it (a curated per-path publish cannot 3-way conflict,
// and the run branch is kept+pushed so output is never stranded),
// but the helper + coordinator endpoint remain for the operator /
// straggler path — so its wire contract still needs coverage.
func TestReportRunSyncConflict_PostsSignal(t *testing.T) {
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

	fc.reportRunSyncConflict(context.Background(), 42, 3,
		"run-1", "main", []string{"data/results.csv", "README.md"},
		"git checkout main && git merge run-1")

	mu.Lock()
	defer mu.Unlock()
	if !postCalled {
		t.Fatal("sync-conflict signal was NOT posted to coord")
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
