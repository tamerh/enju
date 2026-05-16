package service

// FakeExecutor end-to-end test for the SLURM deferred loop
// (spec §7). No cluster: a FakeExecutor stands in for sbatch/
// sacct, and a stub coordinator records the posts. This is the
// one test that drives the whole new path as a unit —
//
//	kickoffWrapTask (executor: slurm, DeferCommit set)
//	  → FakeExecutor.Submit (writes .wrap-job.json, simulates the
//	    node by running compute.Run deferred → .wrap-result.json
//	    with CommitSHA="" + DeferredCommit)
//	→ ReapWrapperFailuresWF
//	  → .wrap-result.json walk: handleOneWrapperResult performs
//	    the REAL host-side compute.CommitDeferred, posts /result
//	  → reapSlurmSidecars: sees .wrap-result.done.json, retires
//	    the sidecar (the §5 gate — must NOT spuriously /fail)
//
// It also covers enju_terminate_run → CancelRunWrappers reaching
// the executor's Cancel.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
	"github.com/enju-ai/enju/internal/fatclient/compute"
	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/executor"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// fakeExecutor implements executor.Executor in-process. Submit
// optionally simulates the SLURM compute node by running
// compute.Run against the real workflow (the serialized spec
// already has DeferCommit=true, so Run produces a
// .wrap-result.json with a DeferredCommit and touches no git).
type fakeExecutor struct {
	wf            *enjugit.Workflow
	env           []string
	produceResult bool

	mu          sync.Mutex
	submitCalls int
	pollCalls   int
	cancelCalls int
}

func (f *fakeExecutor) Submit(ctx context.Context, specPath, outputPath string, env []string, _ enjuYaml.Resources) (executor.Handle, error) {
	f.mu.Lock()
	f.submitCalls++
	f.mu.Unlock()

	h := executor.Handle{Executor: executor.KindSlurm, JobID: "fake-job-1", ResultDir: filepath.Dir(outputPath)}

	if f.produceResult {
		sb, err := os.ReadFile(specPath)
		if err != nil {
			return executor.Handle{}, err
		}
		var spec compute.Spec
		if err := json.Unmarshal(sb, &spec); err != nil {
			return executor.Handle{}, err
		}
		if !spec.DeferCommit {
			// The whole point of executor: slurm — kickoffWrapTask
			// must have set this before serializing.
			panic("fakeExecutor: slurm spec must have DeferCommit=true")
		}
		res := compute.Run(ctx, f.wf, spec, f.env, slog.New(slog.NewTextHandler(io.Discard, nil)))
		if err := compute.WriteResult(outputPath, res); err != nil {
			return executor.Handle{}, err
		}
	}
	// Persist the sidecar exactly as a real executor would so the
	// reaper / CancelRunWrappers can discover it.
	jb, _ := json.MarshalIndent(h, "", "  ")
	if err := os.WriteFile(filepath.Join(h.ResultDir, executor.JobSidecarName), jb, 0o600); err != nil {
		return executor.Handle{}, err
	}
	return h, nil
}

func (f *fakeExecutor) Poll(ctx context.Context, h executor.Handle) (executor.Status, error) {
	f.mu.Lock()
	f.pollCalls++
	f.mu.Unlock()
	// Submit ran synchronously, so the job is always terminal.
	return executor.Status{State: executor.StateDone}, nil
}

func (f *fakeExecutor) Cancel(ctx context.Context, h executor.Handle) error {
	f.mu.Lock()
	f.cancelCalls++
	f.mu.Unlock()
	return nil
}

type postRec struct {
	mu    sync.Mutex
	calls map[string]int
	last  map[string]map[string]interface{}
}

func newPostRec() *postRec {
	return &postRec{calls: map[string]int{}, last: map[string]map[string]interface{}{}}
}

func (p *postRec) record(path string, body map[string]interface{}) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls[path]++
	p.last[path] = body
}

func (p *postRec) count(path string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.calls[path]
}

// stubCoordForSlurm records every POST and answers the few GETs
// OpenWorkflow needs. /result returns "{}" so applyAcceptedMerges
// is a clean no-op (it returns nil when there's no
// accepted_merges key — verified in submit.go).
func stubCoordForSlurm(t *testing.T, rec *postRec, projID int64, bare string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet {
			switch {
			case strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
				_, _ = w.Write([]byte(`{"remote_url":"` + bare + `","name":"p","default_branch":"main"}`))
			case strings.HasPrefix(r.URL.Path, "/api/v1/citizens"):
				_, _ = w.Write([]byte(`{"id":1,"username":"tester","name":"Tester","kind":"human"}`))
			default:
				_, _ = w.Write([]byte(`{}`))
			}
			return
		}
		var body map[string]interface{}
		_ = json.NewDecoder(r.Body).Decode(&body)
		rec.record(r.URL.Path, body)
		_, _ = w.Write([]byte(`{}`))
	})
	ts := httptest.NewServer(mux)
	t.Cleanup(ts.Close)
	return ts
}

func slurmFakeWorkflow(t *testing.T, projID int64) (*enjugit.Workflow, *enjugit.Workspace, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	bare := t.TempDir()
	gittest.InitBareWithSeed(t, bare) // seeds main — CommitDeferred forks the topic from it
	wsRoot := t.TempDir()
	t.Cleanup(func() { chmodWritableTree(t, wsRoot) })
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	projPath := filepath.Join(wsRoot, "p")
	if err := os.MkdirAll(projPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(projectreg.Entry{ID: projID, LocalPath: projPath}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(logger), enjugit.WithRegistry(reg))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	wf, err := ws.ForProject(projID, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	return wf, ws, bare
}

// looksLikeSHA is a cheap "did a real commit happen" check —
// CommitDeferred returns git's own 40-hex object id, so a
// well-formed non-empty SHA means the host-side commit ran (the
// error path returns before any SHA).
func looksLikeSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

// chmodWritableTree makes a workspace tree deletable after a
// run left read-only snapshot dirs (mirrors the compute tests'
// cleanup; named distinctly to avoid colliding with that
// package-private helper).
func chmodWritableTree(t *testing.T, root string) {
	t.Helper()
	_ = filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err == nil && info.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

func trivialScript(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "run.sh")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho slurm-deferred-output\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSlurmDeferredLoopE2E_FakeExecutor(t *testing.T) {
	const projID int64 = 401
	wf, _, bare := slurmFakeWorkflow(t, projID)
	rec := newPostRec()
	ts := stubCoordForSlurm(t, rec, projID, bare)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	fake := &fakeExecutor{wf: wf, env: os.Environ(), produceResult: true}
	fc := &FatClient{
		coord:  coord.New(coord.Config{BaseURL: ts.URL, Username: "tester", Logger: logger}),
		logger: logger,
		executorOverride: func(kind string) (executor.Executor, error) {
			if kind != executor.KindSlurm {
				t.Fatalf("expected slurm dispatch, got %q", kind)
			}
			return fake, nil
		},
	}

	meta := &TaskMeta{Executor: "slurm"}
	spec := compute.Spec{
		TaskID:          "401:1:dc",
		ProjectID:       projID,
		Branch:          "main",
		IterationBranch: "1-test/dc/iter-1",
		ResultDir:       ".enju/runs/1-test/dc",
		ScriptPath:      trivialScript(t),
		ScriptLabel:     "run.sh",
		AuthorName:      "alice",
		AuthorEmail:     "alice@example.com",
		Username:        "alice",
	}
	ctx := context.Background()

	kick, err := fc.kickoffWrapTask(ctx, meta, spec, os.Environ(), spec.ResultDir, wf.WorkDir())
	if err != nil {
		t.Fatalf("kickoffWrapTask: %v", err)
	}
	if kick.Executor != "slurm" || kick.JobID == "" {
		t.Fatalf("kick = %+v, want slurm + non-empty JobID", kick)
	}
	if fake.submitCalls != 1 {
		t.Fatalf("FakeExecutor.Submit calls = %d, want 1", fake.submitCalls)
	}

	resultDir := filepath.Join(wf.WorkDir(), spec.ResultDir)
	// The node deferred: result on disk, CommitSHA empty, payload captured.
	rb, err := os.ReadFile(filepath.Join(resultDir, ".wrap-result.json"))
	if err != nil {
		t.Fatalf("reading .wrap-result.json: %v", err)
	}
	var nodeRes compute.Result
	if err := json.Unmarshal(rb, &nodeRes); err != nil {
		t.Fatalf("decode node result: %v", err)
	}
	if nodeRes.CommitSHA != "" {
		t.Errorf("node must defer: CommitSHA=%q, want empty", nodeRes.CommitSHA)
	}
	if nodeRes.DeferredCommit == nil {
		t.Fatal("node result missing DeferredCommit (reaper would have nothing to replay)")
	}

	// Drive the reaper: host-side commit + /result, then sidecar retired.
	fc.ReapWrapperFailuresWF(ctx, wf)

	if n := rec.count("/api/v1/tasks/401:1:dc/result"); n != 1 {
		t.Fatalf("/result POST count = %d, want 1", n)
	}
	if n := rec.count("/api/v1/tasks/401:1:dc/fail"); n != 0 {
		t.Fatalf("/fail POST count = %d, want 0 (the §5-gate ordering bug)", n)
	}
	body := rec.last["/api/v1/tasks/401:1:dc/result"]
	sha, _ := body["commit_sha"].(string)
	if sha == "" {
		t.Fatalf("/result body missing commit_sha (host-side CommitDeferred did not run): %+v", body)
	}
	// A well-formed git object id ⇒ host-side CommitDeferred ran
	// (its error path returns before producing any SHA).
	if !looksLikeSHA(sha) {
		t.Errorf("commit_sha %q is not a 40-hex git id — host-side commit didn't run", sha)
	}
	// Both markers present → loop fully reaped, idempotent.
	if _, e := os.Stat(filepath.Join(resultDir, ".wrap-result.done.json")); e != nil {
		t.Errorf(".wrap-result.done.json missing: %v", e)
	}
	if _, e := os.Stat(filepath.Join(resultDir, ".wrap-job.done.json")); e != nil {
		t.Errorf(".wrap-job.done.json missing (sidecar not retired): %v", e)
	}

	// Second sweep: nothing left to do, no extra posts.
	fc.ReapWrapperFailuresWF(ctx, wf)
	if n := rec.count("/api/v1/tasks/401:1:dc/result"); n != 1 {
		t.Errorf("second sweep not idempotent: /result count = %d, want still 1", n)
	}
	if n := rec.count("/api/v1/tasks/401:1:dc/fail"); n != 0 {
		t.Errorf("second sweep posted /fail: count = %d, want 0", n)
	}
}

func TestSlurmCancelOnTerminate_FakeExecutor(t *testing.T) {
	const projID int64 = 402
	wf, ws, bare := slurmFakeWorkflow(t, projID)
	rec := newPostRec()
	ts := stubCoordForSlurm(t, rec, projID, bare)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// produceResult=false → an "in-flight" job: sidecar written,
	// no result yet — exactly what terminate must be able to kill.
	fake := &fakeExecutor{wf: wf, env: os.Environ(), produceResult: false}
	fc := &FatClient{
		coord:            coord.New(coord.Config{BaseURL: ts.URL, Username: "tester", Logger: logger}),
		enjugit:          ws,
		logger:           logger,
		executorOverride: func(kind string) (executor.Executor, error) { return fake, nil },
	}

	meta := &TaskMeta{Executor: "slurm"}
	spec := compute.Spec{
		TaskID:          "402:7:dc",
		ProjectID:       projID,
		Branch:          "main",
		IterationBranch: "7-t/dc/iter-1",
		ResultDir:       ".enju/runs/7-t/dc",
		ScriptPath:      trivialScript(t),
		ScriptLabel:     "run.sh",
		AuthorName:      "a",
		AuthorEmail:     "a@e.com",
		Username:        "a",
	}
	ctx := context.Background()
	if _, err := fc.kickoffWrapTask(ctx, meta, spec, os.Environ(), spec.ResultDir, wf.WorkDir()); err != nil {
		t.Fatalf("kickoffWrapTask: %v", err)
	}
	resultDir := filepath.Join(wf.WorkDir(), spec.ResultDir)
	if _, e := os.Stat(filepath.Join(resultDir, executor.JobSidecarName)); e != nil {
		t.Fatalf("sidecar not written by Submit: %v", e)
	}

	// enju_terminate_run path: cancel outstanding wrappers for run 7.
	fc.CancelRunWrappers(ctx, projID, 7)

	if fake.cancelCalls != 1 {
		t.Errorf("FakeExecutor.Cancel calls = %d, want 1 (terminate must reach the executor)", fake.cancelCalls)
	}
	if _, e := os.Stat(filepath.Join(resultDir, ".wrap-job.done.json")); e != nil {
		t.Errorf("sidecar not retired after cancel: %v", e)
	}

	// Scoping: a job in a DIFFERENT run must be untouched.
	fake2 := &fakeExecutor{wf: wf, env: os.Environ(), produceResult: false}
	fc.executorOverride = func(kind string) (executor.Executor, error) { return fake2, nil }
	spec2 := spec
	spec2.TaskID = "402:9:other"
	spec2.ResultDir = ".enju/runs/9-o/other"
	spec2.IterationBranch = "9-o/other/iter-1"
	if _, err := fc.kickoffWrapTask(ctx, meta, spec2, os.Environ(), spec2.ResultDir, wf.WorkDir()); err != nil {
		t.Fatalf("kickoffWrapTask run9: %v", err)
	}
	fc.CancelRunWrappers(ctx, projID, 7) // still run 7
	if fake2.cancelCalls != 0 {
		t.Errorf("run-9 job was cancelled by a run-7 terminate (scope leak): %d", fake2.cancelCalls)
	}
}
