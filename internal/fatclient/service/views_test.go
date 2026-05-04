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
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
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

func TestListMaterializedProjects(t *testing.T) {
	root := t.TempDir()
	// Workspace.projectDir produces "<id>-<slug>" — replicate
	// that shape so the discovery rule round-trips.
	for _, name := range []string{"7-alpha", "11-beta-project", "not-a-project", "garbage"} {
		if err := os.MkdirAll(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// And one file (not a dir) — should be skipped.
	if err := os.WriteFile(filepath.Join(root, "99-file"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws, err := workspace.NewWorkspace(root, logger)
	if err != nil {
		t.Fatal(err)
	}
	fc := New(Config{Workspace: ws, Logger: logger})
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

func TestListMaterializedProjects_NoWorkspace(t *testing.T) {
	fc := New(Config{})
	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != nil {
		t.Errorf("expected nil when workspace nil, got %+v", got)
	}
}

func TestListMaterializedProjects_RegistryWins(t *testing.T) {
	// When the registry has entries, they take priority over the
	// filesystem walk — captures externally adopted dirs that
	// don't live under the workspace root.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	ws, err := workspace.NewWorkspace(root, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Filesystem has one standard clone (id=7) under root.
	if err := os.MkdirAll(filepath.Join(root, "7-alpha"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Registry has a different project (id=99) at an external
	// location. If the filesystem walk runs, we'd see id=7. If
	// the registry wins, we see id=99.
	externalDir := t.TempDir()
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	if err := reg.Upsert(projectreg.Entry{
		ID:        99,
		LocalPath: externalDir,
		Name:      "External",
	}); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{Workspace: ws, Logger: logger, ProjectRegistry: reg})
	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ProjectID != 99 {
		t.Errorf("expected registry to win with id=99, got %+v", got)
	}
}

func TestListMaterializedProjects_FilesystemFallback(t *testing.T) {
	// Empty registry → falls back to filesystem walk. Smooths
	// the migration path for users on older code who have
	// standard clones but no registry yet.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	root := t.TempDir()
	ws, err := workspace.NewWorkspace(root, logger)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(root, "7-alpha"), 0o755); err != nil {
		t.Fatal(err)
	}

	emptyReg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	fc := New(Config{Workspace: ws, Logger: logger, ProjectRegistry: emptyReg})
	got, err := fc.ListMaterializedProjects()
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if len(got) != 1 || got[0].ProjectID != 7 {
		t.Errorf("expected filesystem fallback to find id=7, got %+v", got)
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

func TestParseProjectIDFromDir(t *testing.T) {
	cases := []struct {
		name string
		want int64
	}{
		{"7-alpha", 7},
		{"11-beta-bot", 11},
		{"7", 7},
		{"7abc", 0}, // looks-like-int-but-isn't, no dash to strip
		{"abc-7", 0},
		{"-7", 0},
		{"", 0},
	}
	for _, tc := range cases {
		if got := parseProjectIDFromDir(tc.name); got != tc.want {
			t.Errorf("parseProjectIDFromDir(%q) = %d, want %d", tc.name, got, tc.want)
		}
	}
}
