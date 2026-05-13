package service

// End-to-end self-healing tests for FatClient.reportMerge.
//
// The unit tests in report_merge_test.go prove the HTTP retry
// layer recovers a 503 against a canned mock. They do NOT prove
// that a duplicate /merges POST against a REAL coordinator no-ops
// cleanly — i.e. that the SUBMITTED → ACCEPTED transition + the
// branch_merged audit event each fire exactly once across the
// retry cycle. That idempotency property is the load-bearing piece
// the silent-stall fix relies on; pre-fix, a failed POST silently
// dropped the state flip, leaving the topic in origin and the task
// wedged at SUBMITTED forever.
//
// Two tests, one per failure shape:
//
//   - drop-before-handler (Test 1): the proxy returns 503 without
//     forwarding. Coord sees only the retry. Canonical "503 from
//     an overloaded coord" path that load testing surfaced.
//
//   - forward-then-drop-response (Test 2): the proxy forwards to
//     coord (state flip + branch_merged land), then returns 503 to
//     the fat-client. Fat-client retries; coord sees the duplicate
//     and hits the state guard at coordinator/service/report_merge.go.
//     Test 2 is the load-bearing idempotency assertion — flipping
//     the state guard at coord-side report_merge.go to a no-op
//     causes Test 2 to fail with "expected exactly one
//     branch_merged event, got two."
//
// Setup is shared: a real coordinator (api.NewServer + httptest.Server),
// SQLite state + events DBs in a temp dir, a single SUBMITTED task
// planted via store.ApplyPlan, and a flaky reverse-proxy in front
// of the coord that the fat-client points at.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/api"
	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/store"
	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// e2eFixture bundles the planted state + the live coord URL the
// flaky proxy will forward to. Every test gets its own fixture in
// its own temp directory so parallel runs don't collide on
// SQLite files.
type e2eFixture struct {
	store     *store.Store
	coordURL  string
	coordSrv  *httptest.Server
	citizenID int64
	token     string
	username  string
	projectID int64
	runID     int64
	runSeq    int
	taskID    string
	topic     string
	runBranch string
	mergeSHA  string
}

// newE2EFixture stands up a real coordinator backed by SQLite,
// registers a citizen + project + run + a single answer task in
// SUBMITTED state. Returns the live state needed to drive
// /merges through a flaky proxy.
//
// State pipeline:
//
// 1. /citizens/register over HTTP — only public path, gives us a
// valid Bearer token the auth middleware accepts.
// 2. ApplyPlan(CreateProject + AddProjectMember) — direct store
// mutation; bypasses the create-project HTTP handler's git
// bare bring-up since this test never touches a real repo.
// 3. ApplyPlan(CreateRun) — minimal active run on `main`.
// 4. ApplyPlan(CreateTask + SetClaim + RecordSubmission) — drives
// the task through ready → claimed → submitted in three
// transactions. RecordSubmission's UPDATE inside
// applyRecordSubmission is the actual "land in SUBMITTED"
// edge.
func newE2EFixture(t *testing.T) *e2eFixture {
	t.Helper()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	st, err := store.New(filepath.Join(dir, "state.db"))
	if err != nil {
		t.Fatalf("store.New: %v", err)
	}
	t.Cleanup(func() { st.Close() })

	es, err := store.NewSQLiteEventStore(filepath.Join(dir, "events.db"), logger)
	if err != nil {
		t.Fatalf("NewSQLiteEventStore: %v", err)
	}
	t.Cleanup(func() { es.Close() })
	st.AttachEventStore(es)

	srv := api.NewServer(st, logger)
	coordSrv := httptest.NewServer(srv.Router())
	t.Cleanup(coordSrv.Close)

	// /citizens/register through the real handler so we get back
	// the same {citizen_id, token} shape an MCP client would. The
	// auth middleware will recognize this token from the citizens
	// table on every subsequent request.
	regBody, _ := json.Marshal(map[string]string{
		"username": "merge-e2e",
		"name":     "Merge E2E",
	})
	regResp, err := http.Post(coordSrv.URL+"/api/v1/citizens/register",
		"application/json", bytes.NewReader(regBody))
	if err != nil {
		t.Fatalf("register POST: %v", err)
	}
	defer regResp.Body.Close()
	if regResp.StatusCode != http.StatusOK && regResp.StatusCode != http.StatusCreated {
		body, _ := io.ReadAll(regResp.Body)
		t.Fatalf("register status=%d body=%s", regResp.StatusCode, body)
	}
	var reg struct {
		ID    int64  `json:"id"`
		Token string `json:"token"`
	}
	if err := json.NewDecoder(regResp.Body).Decode(&reg); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	if reg.Token == "" || reg.ID == 0 {
		t.Fatalf("register returned empty creds: %+v", reg)
	}

	now := time.Now()
	projRes, err := st.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateProject{Project: store.ProjectRecord{
				Name:          "merge-e2e",
				CreatedBy:     "merge-e2e",
				DefaultBranch: "main",
				CreatedAt:     now,
				UpdatedAt:     now,
			}},
		},
	})
	if err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	projectID := projRes.ProjectID
	if _, err := st.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.AddProjectMember{
				ProjectID: projectID,
				CitizenID: reg.ID,
				Role:      store.ProjectRoleOwner,
			},
		},
	}); err != nil {
		t.Fatalf("AddProjectMember: %v", err)
	}

	runRes, err := st.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateRun{Run: store.RunRecord{
				ProjectID: projectID,
				Name:      "r1",
				YAMLData:  "name: r1",
				State:     store.RunActive,
				Branch:    "main",
				Slug:      "r1",
				CreatedAt: now,
				UpdatedAt: now,
			}},
		},
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	runID, runSeq := runRes.RunID, runRes.RunSeq

	taskID := fmt.Sprintf("%d:%d:write", projectID, runSeq)
	if _, err := st.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.CreateTask{Task: store.TaskRecord{
				ID:         taskID,
				RunID:      runID,
				Seq:        1,
				TaskDefID:  "write",
				Action:     "answer",
				ResultType: "text",
				State:      store.TaskReady,
				RunSlug:    "r1",
				CreatedAt:  now,
			}},
		},
	}); err != nil {
		t.Fatalf("CreateTask: %v", err)
	}

	if _, err := st.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.SetClaim{
				TaskID:    taskID,
				CitizenID: reg.ID,
				Deadline:  now.Add(time.Hour),
			},
		},
	}); err != nil {
		t.Fatalf("SetClaim: %v", err)
	}

	commitSHA := strings.Repeat("a", 40)
	if _, err := st.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.RecordSubmission{
				TaskID:     taskID,
				CitizenID:  reg.ID,
				CommitSHA:  commitSHA,
				ResultPath: ".enju/runs/1-r1/write",
				TokensUsed: 10,
			},
		},
	}); err != nil {
		t.Fatalf("RecordSubmission: %v", err)
	}

	// Sanity-check: the task must be in SUBMITTED before /merges
	// can flip it to ACCEPTED. If RecordSubmission landed somewhere
	// else (e.g. inline-accepted by some future change) the
	// idempotency story we're testing doesn't apply and we want a
	// loud failure here, not a silent pass.
	tk, err := st.GetTask(taskID)
	if err != nil || tk == nil {
		t.Fatalf("GetTask after submit: tk=%+v err=%v", tk, err)
	}
	if got := store.TaskState(tk.State); got != store.TaskSubmitted {
		t.Fatalf("task should be SUBMITTED post-RecordSubmission, got %q", got)
	}

	return &e2eFixture{
		store:     st,
		coordURL:  coordSrv.URL,
		coordSrv:  coordSrv,
		citizenID: reg.ID,
		token:     reg.Token,
		username:  "merge-e2e",
		projectID: projectID,
		runID:     runID,
		runSeq:    runSeq,
		taskID:    taskID,
		topic:     "topic/run-1/write",
		runBranch: "main",
		mergeSHA:  strings.Repeat("b", 40),
	}
}

// flakyMode picks the proxy's failure shape for the first /merges
// POST. Subsequent POSTs always pass through cleanly.
type flakyMode int

const (
	// dropBeforeHandler — the proxy returns 503 to the fat-client
	// without ever forwarding the request to coord. Coord sees
	// only the retry. Models the "request dropped at the network
	// edge" failure mode.
	dropBeforeHandler flakyMode = iota
	// forwardThenDropResponse — the proxy forwards the request to
	// coord (which fully processes it: state flip, branch_merged
	// emit), then returns 503 to the fat-client. The retry hits
	// coord's idempotent state guard. Models the "coord did the
	// work, but the response got eaten on the way back" failure.
	forwardThenDropResponse
)

// flakyProxy is the test double for a transient failure on the
// fat-client → coord round-trip. It wraps a normal reverse-proxy
// but intercepts /merges POSTs against a target task and applies
// the configured failure shape on the FIRST matching request.
type flakyProxy struct {
	t        *testing.T
	mode     flakyMode
	taskID   string
	rp       *httputil.ReverseProxy
	coord    *url.URL
	hits     atomic.Int32 // total /merges POSTs the proxy observed
	dropped  atomic.Int32 // count of failure-shape applications
	maxDrops int32        // cap on failure injections (default 1)
}

func newFlakyProxy(t *testing.T, coordURL string, mode flakyMode, taskID string) *httptest.Server {
	t.Helper()
	cu, err := url.Parse(coordURL)
	if err != nil {
		t.Fatalf("parse coord URL: %v", err)
	}
	fp := &flakyProxy{
		t:        t,
		mode:     mode,
		taskID:   taskID,
		rp:       httputil.NewSingleHostReverseProxy(cu),
		coord:    cu,
		maxDrops: 1,
	}
	srv := httptest.NewServer(fp)
	t.Cleanup(srv.Close)
	return srv
}

func (f *flakyProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Pass everything that isn't a /merges POST straight through.
	// The fat-client only contacts coord on /merges in this test,
	// but be defensive: a future refactor that adds a probe call
	// before reportMerge shouldn't accidentally trigger the
	// failure injector.
	if !strings.HasSuffix(r.URL.Path, "/merges") || r.Method != http.MethodPost {
		f.rp.ServeHTTP(w, r)
		return
	}

	// Read the body once so we can both peek at task_id (to gate
	// the failure on the right call) and replay it whether we
	// forward or drop. Buffering is fine — /merges bodies are tiny
	// (~150 bytes) and there's only ever a handful per test.
	body, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "proxy read body: "+err.Error(), http.StatusBadGateway)
		return
	}
	r.Body.Close()

	matches := bodyMatchesTask(body, f.taskID)
	if !matches {
		// Different /merges POST — replay body and forward
		// without counting it against our hit ledger. This test's
		// fixture only ever drives one task so in practice this
		// branch is dead, but it keeps the proxy correct if a
		// future variant runs more.
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		f.rp.ServeHTTP(w, r)
		return
	}

	f.hits.Add(1)
	if f.dropped.Load() < f.maxDrops {
		f.dropped.Add(1)
		switch f.mode {
		case dropBeforeHandler:
			// Coord never sees this request. The fat-client's
			// retry will be the only POST that lands.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "proxy: dropped before handler"}`))
			return
		case forwardThenDropResponse:
			// Forward to coord with a fresh request, discard the
			// real response, and return 503 to the caller. The
			// fat-client's retry will be a duplicate that hits
			// coord's state guard.
			fwd, ferr := http.NewRequestWithContext(r.Context(), r.Method,
				f.coord.String()+r.URL.RequestURI(), bytes.NewReader(body))
			if ferr != nil {
				http.Error(w, "proxy: build forwarded req: "+ferr.Error(), http.StatusBadGateway)
				return
			}
			fwd.Header = r.Header.Clone()
			resp, ferr := http.DefaultClient.Do(fwd)
			if ferr != nil {
				http.Error(w, "proxy: forward failed: "+ferr.Error(), http.StatusBadGateway)
				return
			}
			// Drain + close so coord's writer goroutine sees a
			// clean response cycle even though the fat-client
			// will never see the body.
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()

			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "proxy: response dropped"}`))
			return
		}
	}

	// Pass-through path: replay buffered body so the reverse-
	// proxy can re-read it.
	r.Body = io.NopCloser(bytes.NewReader(body))
	r.ContentLength = int64(len(body))
	f.rp.ServeHTTP(w, r)
}

// bodyMatchesTask checks the JSON body's task_id field against the
// target. The proxy gates failure injection on this so other
// tests (or a hypothetical future setup with multiple tasks)
// don't accidentally trip the same failure.
func bodyMatchesTask(body []byte, target string) bool {
	if target == "" {
		return true
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		return false
	}
	id, _ := m["task_id"].(string)
	return id == target
}

// shrinkRetryBackoffs swaps the package-level retry schedule for
// fast millisecond sleeps and restores it on cleanup. Mirrors what
// the unit tests in report_merge_test.go do; one retry attempt
// (the success path) is all the e2e tests need.
func shrinkRetryBackoffs(t *testing.T) {
	t.Helper()
	saved := reportMergeRetryBackoffs
	reportMergeRetryBackoffs = []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
	}
	t.Cleanup(func() { reportMergeRetryBackoffs = saved })
}

// countEvents returns how many events of `eventType` exist for the
// run, after waiting for the async event-store writer to flush.
// The exact-one assertions in both tests are the load-bearing
// signal — they're what would fail if a future refactor lost the
// idempotency property under test.
func countEvents(t *testing.T, st *store.Store, runID int64, eventType string) int {
	t.Helper()
	st.Events().WaitForDrain(2 * time.Second)
	events, err := st.ListEvents(store.EventQuery{
		RunID:      runID,
		EventTypes: []string{eventType},
		Limit:      100,
	})
	if err != nil {
		t.Fatalf("ListEvents(%s): %v", eventType, err)
	}
	return len(events)
}

// TestReportMergeE2E_DropBeforeHandler_SelfHeals covers the
// canonical "fat-client retry recovers a request that never
// reached coord" path. Asserts the retry actually fired (two
// proxy hits), the underlying state flip + branch_merged event
// each landed exactly once, and applyAcceptedMerges returned nil.
//
// Without the retry loop, this test would fail at "exactly two
// POSTs" → "got 1" AND at "task accepted" → "got submitted",
// because the first 503 returned to the fat-client and the
// pipeline gave up.
func TestReportMergeE2E_DropBeforeHandler_SelfHeals(t *testing.T) {
	shrinkRetryBackoffs(t)
	fx := newE2EFixture(t)

	proxy := newFlakyProxy(t, fx.coordURL, dropBeforeHandler, fx.taskID)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := coord.New(coord.Config{
		BaseURL:   proxy.URL,
		Username:  fx.username,
		AuthToken: fx.token,
		Logger:    logger,
	})
	fc := New(Config{Coord: c, Logger: logger})

	if err := fc.reportMerge(context.Background(),
		fx.projectID, int64(fx.runSeq),
		fx.taskID, fx.topic, fx.runBranch, fx.mergeSHA); err != nil {
		t.Fatalf("reportMerge after retry must succeed, got: %v", err)
	}

	// Coord state — task must have advanced past SUBMITTED.
	tk, err := fx.store.GetTask(fx.taskID)
	if err != nil || tk == nil {
		t.Fatalf("GetTask: tk=%+v err=%v", tk, err)
	}
	if got := store.TaskState(tk.State); got != store.TaskAccepted {
		t.Errorf("task state = %q, want accepted (the /merges retry should have flipped it)", got)
	}

	if got := countEvents(t, fx.store, fx.runID, "branch_merged"); got != 1 {
		t.Errorf("branch_merged event count = %d, want exactly 1", got)
	}
	if got := countEvents(t, fx.store, fx.runID, "task_completed"); got != 1 {
		t.Errorf("task_completed event count = %d, want exactly 1", got)
	}

	// Proxy ledger — exactly one drop and one pass-through. The
	// fat-client should have made exactly two attempts; the first
	// failed, the second succeeded.
	target := proxy.Config.Handler.(*flakyProxy)
	if got := target.hits.Load(); got != 2 {
		t.Errorf("proxy /merges hits = %d, want 2 (1 dropped + 1 retry)", got)
	}
	if got := target.dropped.Load(); got != 1 {
		t.Errorf("proxy drops = %d, want 1", got)
	}
}

// TestReportMergeE2E_ForwardThenDropResponse_Idempotent is the
// load-bearing idempotency assertion. The proxy lets the first
// /merges POST reach coord (state flips to ACCEPTED, branch_merged
// emits) and then drops the RESPONSE on the way back. The fat-
// client's retry posts a duplicate; coord's state guard at
// internal/coordinator/service/report_merge.go (the "task already
// past SUBMITTED → skip" branch) makes it a no-op for both the
// state flip AND the audit event.
//
// Idempotency model: branch_merged is emitted INSIDE the state
// guard, co-fired with acceptTask. So a /merges retry that finds
// the task already past SUBMITTED skips both — exactly one
// branch_merged row, exactly one task_completed event, no
// double-cascade. The retry's "we tried again" signal lives on
// the per-receipt INFO log line, which is the right channel for
// that diagnostic. See report_merge.go's idempotency-contract
// comment for the full rationale.
//
// Removing the state guard at coord-side report_merge.go would
// fail this test on both branch_merged (2 instead of 1) and
// task_completed (2 instead of 1) — confirming the test catches
// the regression class it claims to.
func TestReportMergeE2E_ForwardThenDropResponse_Idempotent(t *testing.T) {
	shrinkRetryBackoffs(t)
	fx := newE2EFixture(t)

	proxy := newFlakyProxy(t, fx.coordURL, forwardThenDropResponse, fx.taskID)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := coord.New(coord.Config{
		BaseURL:   proxy.URL,
		Username:  fx.username,
		AuthToken: fx.token,
		Logger:    logger,
	})
	fc := New(Config{Coord: c, Logger: logger})

	if err := fc.reportMerge(context.Background(),
		fx.projectID, int64(fx.runSeq),
		fx.taskID, fx.topic, fx.runBranch, fx.mergeSHA); err != nil {
		t.Fatalf("reportMerge should self-heal via retry, got: %v", err)
	}

	tk, err := fx.store.GetTask(fx.taskID)
	if err != nil || tk == nil {
		t.Fatalf("GetTask: tk=%+v err=%v", tk, err)
	}
	if got := store.TaskState(tk.State); got != store.TaskAccepted {
		t.Errorf("task state = %q, want accepted", got)
	}

	if got := countEvents(t, fx.store, fx.runID, "branch_merged"); got != 1 {
		t.Errorf("branch_merged event count = %d, want exactly 1 — "+
			"a duplicate /merges POST after the first one already "+
			"landed must NOT double-emit the audit event", got)
	}
	if got := countEvents(t, fx.store, fx.runID, "task_completed"); got != 1 {
		t.Errorf("task_completed event count = %d, want exactly 1 — "+
			"the state guard at coord-side report_merge.go must "+
			"skip the second acceptTask call cleanly", got)
	}

	target := proxy.Config.Handler.(*flakyProxy)
	if got := target.hits.Load(); got != 2 {
		t.Errorf("proxy /merges hits = %d, want 2 (both forwarded; "+
			"first response dropped, second passed through)", got)
	}
	if got := target.dropped.Load(); got != 1 {
		t.Errorf("proxy drops = %d, want 1", got)
	}
}
