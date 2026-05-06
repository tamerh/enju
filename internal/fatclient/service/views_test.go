package service

// Tests for the read-side view methods. Each test stands up a
// httptest.NewServer that responds to the specific coord paths
// with canned JSON, points a fat-client coord.Client at it, and
// asserts the typed view models come back with the right
// fields. Mocking the coord at the HTTP layer (vs. mocking the
// FatClient itself) keeps the JSON-decode path under test —
// where wire-format drift will actually bite.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

// fakeCoord builds a httptest server that returns canned
// responses for the request paths in `routes`. Unmatched paths
// return 404. Use it from each test so each test owns its
// fixtures end-to-end (no shared state, no mystery guests).
func fakeCoord(t *testing.T, routes map[string]any) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for path, body := range routes {
		path := path
		body := body
		mux.HandleFunc(path, func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			if err := json.NewEncoder(w).Encode(body); err != nil {
				t.Fatalf("encode %s: %v", path, err)
			}
		})
	}
	return httptest.NewServer(mux)
}

// newViewClient is the test harness — points a real coord.Client
// at the test server, wraps it in a real FatClient. Same
// composition production uses.
func newViewClient(t *testing.T, baseURL string) *FatClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := coord.New(coord.Config{
		BaseURL:   baseURL,
		Username:  "tamer",
		AuthToken: "test-token",
		Logger:    logger,
	})
	return New(Config{Coord: c, Logger: logger})
}

func TestListProjects(t *testing.T) {
	srv := fakeCoord(t, map[string]any{
		"/api/v1/projects": []map[string]any{
			{
				"id":             int64(7),
				"name":           "Alpha",
				"description":    "first",
				"remote_url":     "git@example.com:alpha.git",
				"default_branch": "main",
				"run_count":      3,
				"created_at":     "2026-05-01T10:00:00Z",
			},
			{
				"id":             int64(11),
				"name":           "Beta",
				"default_branch": "main",
				"run_count":      0,
				"created_at":     "2026-05-02T10:00:00Z",
			},
		},
	})
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	got, err := fc.ListProjects(context.Background())
	if err != nil {
		t.Fatalf("ListProjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 projects, got %d", len(got))
	}
	if got[0].ID != 7 || got[0].Name != "Alpha" || got[0].RunCount != 3 {
		t.Errorf("project[0] mismatch: %+v", got[0])
	}
	if got[0].CreatedAt.IsZero() {
		t.Errorf("project[0].CreatedAt did not parse: %q", got[0].CreatedAt)
	}
	if got[1].Description != "" {
		t.Errorf("project[1] description should be empty, got %q", got[1].Description)
	}
}

func TestGetProject(t *testing.T) {
	srv := fakeCoord(t, map[string]any{
		"/api/v1/projects/7": map[string]any{
			"id":             int64(7),
			"name":           "Alpha",
			"default_branch": "main",
			"run_count":      3,
			"created_at":     "2026-05-01T10:00:00Z",
		},
		"/api/v1/projects/7/members": []map[string]any{
			{
				"username": "tamer",
				"name":     "Tamer",
				"role":     "owner",
				"added_at": "2026-04-15T09:00:00Z",
			},
			{
				"username": "claude",
				"role":     "member",
				"added_at": "2026-04-20T09:00:00Z",
				"added_by": "tamer",
			},
		},
	})
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	got, err := fc.GetProject(context.Background(), 7)
	if err != nil {
		t.Fatalf("GetProject: %v", err)
	}
	if got.ID != 7 || got.Name != "Alpha" {
		t.Errorf("project mismatch: %+v", got.Project)
	}
	if len(got.Members) != 2 {
		t.Fatalf("want 2 members, got %d", len(got.Members))
	}
	if got.Members[0].Username != "tamer" || got.Members[0].Role != "owner" {
		t.Errorf("members[0] mismatch: %+v", got.Members[0])
	}
	if got.Members[1].AddedBy != "tamer" {
		t.Errorf("members[1].AddedBy mismatch: %q", got.Members[1].AddedBy)
	}
}

func TestListRuns(t *testing.T) {
	srv := fakeCoord(t, map[string]any{
		"/api/v1/projects/7/runs": []map[string]any{
			{
				"id":         int64(101),
				"project_id": int64(7),
				"seq":        1,
				"name":       "draft",
				"state":      "completed",
				"branch":     "main",
				"slug":       "draft",
				"task_count": 5,
				"created_at": "2026-05-01T10:00:00Z",
			},
			{
				"id":         int64(102),
				"project_id": int64(7),
				"seq":        2,
				"state":      "in_progress",
				"task_count": 3,
				"created_at": "2026-05-02T10:00:00Z",
			},
		},
	})
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	got, err := fc.ListRuns(context.Background(), 7)
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 runs, got %d", len(got))
	}
	if got[0].State != "completed" || got[0].TaskCount != 5 {
		t.Errorf("run[0] mismatch: %+v", got[0])
	}
}

func TestGetRun(t *testing.T) {
	srv := fakeCoord(t, map[string]any{
		"/api/v1/projects/7/runs/2": map[string]any{
			"id":         int64(102),
			"project_id": int64(7),
			"seq":        2,
			"name":       "review",
			"state":      "in_progress",
			"branch":     "feature-x",
			"task_count": 2,
			"created_at": "2026-05-02T10:00:00Z",
		},
		"/api/v1/projects/7/runs/2/tasks": []map[string]any{
			{
				"id":         "7:2:draft",
				"action":     "draft",
				"state":      "completed",
				"seq":        1,
				"assign_to":  []string{"tamer"},
				"claimed_by": "tamer",
			},
			{
				"id":         "7:2:review",
				"action":     "review",
				"state":      "ready",
				"seq":        2,
				"depends_on": "7:2:draft",
			},
		},
	})
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	got, err := fc.GetRun(context.Background(), 7, 2)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.Seq != 2 || got.State != "in_progress" {
		t.Errorf("run mismatch: %+v", got.Run)
	}
	if len(got.Tasks) != 2 {
		t.Fatalf("want 2 tasks, got %d", len(got.Tasks))
	}
	if got.Tasks[0].ClaimedBy != "tamer" || got.Tasks[0].AssignedTo[0] != "tamer" {
		t.Errorf("task[0] mismatch: %+v", got.Tasks[0])
	}
	// DependsOn is comma-separated on the wire; should split into a slice.
	if len(got.Tasks[1].DependsOn) != 1 || got.Tasks[1].DependsOn[0] != "7:2:draft" {
		t.Errorf("task[1].DependsOn parse mismatch: %v", got.Tasks[1].DependsOn)
	}
	// Mermaid is a pure render — content is exercised by format/
	// tests; here we just confirm it's non-empty so wiring is alive.
	if got.DiagramMermaid == "" {
		t.Errorf("DiagramMermaid empty — render didn't fire")
	}
}

func TestListEvents_BuildsQueryString(t *testing.T) {
	var seenQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/7/events", func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"seq":     int64(42),
				"ts":      "2026-05-04T12:00:00Z",
				"type":    "task_completed",
				"task_id": "7:2:draft",
				"citizen": "tamer",
				"metadata": map[string]any{
					"verdict": "approve",
				},
			},
			{
				// Pin the assign_to top-level hoist contract: the
				// coord side lifts metadata.assign_to onto the row
				// so consumers don't have to parse metadata
				// themselves. If the coord ever drops the hoist,
				// this row breaks and we know.
				"seq":       int64(43),
				"ts":        "2026-05-04T12:00:01Z",
				"type":      "task_request_changes",
				"task_id":   "7:2:draft",
				"citizen":   "alice",
				"assign_to": "alice",
				"metadata":  map[string]any{"assign_to": "alice"},
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	// Set Since to a recent time — exercises the since= query
	// path and the time.Time field, killing the unused-import
	// dance the test file used to need.
	since := time.Now().Add(-1 * time.Hour)
	got, err := fc.ListEvents(context.Background(), 7, ListEventsOpts{
		Since:      since,
		SinceSeq:   42,
		Limit:      10,
		EventTypes: []string{"task_completed", "task_submitted"},
		RunSeq:     2,
	})
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(got) != 2 || got[0].Seq != 42 {
		t.Fatalf("event mismatch: %+v", got)
	}
	if got[0].Metadata["verdict"] != "approve" {
		t.Errorf("metadata not parsed: %+v", got[0].Metadata)
	}
	if got[1].AssignTo != "alice" {
		t.Errorf("assign_to top-level hoist not preserved: %+v", got[1])
	}
	// Query string should carry every option that was set.
	for _, want := range []string{"since=", "since_seq=42", "limit=10", "event_types=task_completed%2Ctask_submitted", "run_seq=2"} {
		if !strings.Contains(seenQuery, want) {
			t.Errorf("query %q missing %q", seenQuery, want)
		}
	}
}

func TestListEvents_NoOptsNoQueryString(t *testing.T) {
	var seenQuery string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/7/events", func(w http.ResponseWriter, r *http.Request) {
		seenQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("[]"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	if _, err := fc.ListEvents(context.Background(), 7, ListEventsOpts{}); err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if seenQuery != "" {
		t.Errorf("expected empty query, got %q", seenQuery)
	}
}

func TestListReadyTasks_ScopedToRun(t *testing.T) {
	// Pin the wire shape: with runID > 0 the daemon's bot loop
	// expects /api/v1/tasks/ready?project_id=7&run_id=10 and a
	// flat array back. Drift on either side breaks the bot
	// daemon silently — this test fails loudly first.
	srv := fakeCoord(t, map[string]any{
		"/api/v1/tasks/ready": []map[string]any{
			{
				"id":         "7:10:review-design",
				"action":     "review",
				"assign_to":  []any{"reviewer-bot"},
				"seq":        float64(1),
				"claimed_by": "",
			},
			{
				"id":        "7:10:answer-q1",
				"action":    "answer",
				"assign_to": []any{},
				"seq":       float64(2),
			},
		},
	})
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	got, err := fc.ListReadyTasks(context.Background(), 7, 10)
	if err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 tasks, got %d: %+v", len(got), got)
	}
	if id, _ := got[0]["id"].(string); id != "7:10:review-design" {
		t.Errorf("first task id: got %q", id)
	}
	if act, _ := got[1]["action"].(string); act != "answer" {
		t.Errorf("second task action: got %q", act)
	}
}

func TestListReadyTasks_RunIDZeroOmitsRunIDQuery(t *testing.T) {
	// runID == 0 should query without &run_id=, fetching every
	// run's ready tasks. The fakeCoord matches by exact path,
	// so the test asserts the path shape via the Server seeing
	// the request — we capture it via the handler.
	var sawPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawPath = r.URL.RequestURI()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	if _, err := fc.ListReadyTasks(context.Background(), 7, 0); err != nil {
		t.Fatalf("ListReadyTasks: %v", err)
	}
	want := "/api/v1/tasks/ready?project_id=7"
	if sawPath != want {
		t.Errorf("path: got %q, want %q (no run_id when runID==0)", sawPath, want)
	}
}

func TestListReadyTasks_DecodeError(t *testing.T) {
	// Server returns non-array JSON without an "error" field.
	// Method should surface the decode error, not panic.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"oops": "not an array"}`))
	}))
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	_, err := fc.ListReadyTasks(context.Background(), 7, 10)
	if err == nil {
		t.Fatal("expected decode error on non-array response")
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Errorf("error message: got %q, want substring \"decoding\"", err.Error())
	}
}

// Pin the bug-fix from the membership story: when the coord
// returns a 4xx with `{"error": "not a member of this project"}`,
// the view method must surface the coord's message verbatim
// rather than the misleading JSON-decode error that came from
// trying to unmarshal the envelope into the typed slice. This
// is what produced the "decode runs: cannot unmarshal object
// into Go value of type []wire.Run" mystery in production.
func TestListRuns_SurfacesCoordErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": "not a member of this project"}`))
	}))
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	_, err := fc.ListRuns(context.Background(), 7)
	if err == nil {
		t.Fatal("expected error from 403 response")
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Errorf("error should carry coord message verbatim, got: %v", err)
	}
	if strings.Contains(err.Error(), "decode") {
		t.Errorf("error should NOT mention JSON decode (that's the bug being fixed): %v", err)
	}
}

func TestListProjects_SurfacesCoordErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error": "stale token"}`))
	}))
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	_, err := fc.ListProjects(context.Background())
	if err == nil {
		t.Fatal("expected error from 401 response")
	}
	if !strings.Contains(err.Error(), "stale token") {
		t.Errorf("error should carry coord message: %v", err)
	}
}

func TestListReadyTasks_SurfacesCoordErrorEnvelope(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": "not a member of this project"}`))
	}))
	defer srv.Close()

	fc := newViewClient(t, srv.URL)
	_, err := fc.ListReadyTasks(context.Background(), 7, 10)
	if err == nil {
		t.Fatal("expected error from 403 response")
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Errorf("error should carry coord message: %v", err)
	}
}

func TestListMaterializedProjects(t *testing.T) {
	// Post-Phase-A every project home is registered explicitly,
	// so this just round-trips through the registry. Two
	// projects in, two projects out.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	dir7 := t.TempDir()
	dir11 := t.TempDir()
	if err := reg.Upsert(projectreg.Entry{ID: 7, LocalPath: dir7, Name: "alpha"}); err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(projectreg.Entry{ID: 11, LocalPath: dir11, Name: "beta-project"}); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{Logger: logger, ProjectRegistry: reg})
	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("ListMaterializedProjects: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 materialized projects, got %d: %+v", len(got), got)
	}
	ids := map[int64]bool{}
	for _, p := range got {
		ids[p.ProjectID] = true
	}
	if !ids[7] || !ids[11] {
		t.Errorf("expected ids 7 and 11, got %+v", got)
	}
}

func TestListMaterializedProjects_NoRegistry(t *testing.T) {
	fc := New(Config{})
	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil when workspace nil, got %+v", got)
	}
}

func TestListMaterializedProjects_FromRegistry(t *testing.T) {
	// Post-Phase-A every project has an explicit registry entry
	// (path is required at create_project + init time), so the
	// registry is the only source ListMaterializedProjects
	// consults. Pre-refactor this test also exercised a
	// filesystem-walk fallback (deleted in Phase D once the
	// "managed workspace dir" concept went away).
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	externalDir := t.TempDir()
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	if err := reg.Upsert(projectreg.Entry{
		ID:        99,
		LocalPath: externalDir,
		Name:      "External",
	}); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{Logger: logger, ProjectRegistry: reg})
	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ProjectID != 99 || got[0].Path != externalDir {
		t.Errorf("expected single registry entry id=99 path=%q, got %+v", externalDir, got)
	}
}

func TestRegisterProject_NoRegistryNoOp(t *testing.T) {
	// Without a registry, RegisterProject silently no-ops.
	fc := New(Config{})
	fc.RegisterProject(projectreg.Entry{ID: 7, LocalPath: t.TempDir()})
	// No assertion beyond "doesn't panic" — the no-op contract
	// is what handlers rely on so they don't need to nil-check.
}

func TestRegisterAndList_RoundTrip(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	dir := t.TempDir()

	fc := New(Config{Logger: logger, ProjectRegistry: reg})
	fc.RegisterProject(projectreg.Entry{
		ID:        7,
		LocalPath: dir,
		Name:      "Alpha",
		RemoteURL: "git@example.com:alpha.git",
	})

	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ProjectID != 7 || got[0].Path != dir {
		t.Errorf("round-trip mismatch: %+v", got)
	}
}

// (TestParseProjectIDFromDir removed in Phase D — the helper
// only existed to parse "<id>-<slug>" workspace-managed dir
// names, which the layout refactor eliminated.)
