package mcpserver

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/enju-ai/enju/internal/mcpgit"
)

func newAPIClient(baseURL string) *apiClient {
	return &apiClient{
		baseURL:    baseURL,
		username:   "tester",
		httpClient: &http.Client{},
		logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

// TestFetchTaskMetaFullPayload verifies every field on taskMeta gets
// populated when the coordinator returns a rich task record.
func TestFetchTaskMetaFullPayload(t *testing.T) {
	body := `{
		"project_id": 7,
		"project_remote_url": "git@example.com:p.git",
		"project_name": "Demo",
		"run_seq": 3,
		"task_def_id": "step1",
		"instance_key": "alpha",
		"outputs": "{\"summary\":{\"format\":\"text\"}}",
		"action": "compute",
		"reviews_target": "",
		"vote_options": "",
		"state": "ready",
		"citizens": 2,
		"script": "scripts/run.sh",
		"writes_artifacts": ["out/a.md", "out/b.md"],
		"run_source_path": "enju/templates/demo",
		"run_params": {"k": "v"},
		"instance_params_map": {"stem": "alpha"},
		"run_branch": "feature-x",
		"env": {"FOO": "bar", "BAZ": "qux"},
		"mode": "async"
	}`
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks/t-1" {
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(body))
	}))
	defer ts.Close()

	c := newAPIClient(ts.URL)
	meta, err := c.fetchTaskMeta(context.Background(), "t-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "t-1" || meta.ProjectID != 7 || meta.RunSeq != 3 {
		t.Fatalf("basic fields wrong: %+v", meta)
	}
	if meta.ProjectRemoteURL != "git@example.com:p.git" || meta.ProjectName != "Demo" {
		t.Fatalf("project fields wrong: %+v", meta)
	}
	if meta.TaskDefID != "step1" || meta.InstanceKey != "alpha" {
		t.Fatalf("def/instance wrong: %+v", meta)
	}
	if meta.Action != "compute" || meta.State != "ready" || meta.Mode != "async" {
		t.Fatalf("state/action/mode wrong: %+v", meta)
	}
	if meta.Citizens != 2 || meta.Script != "scripts/run.sh" {
		t.Fatalf("citizens/script wrong: %+v", meta)
	}
	if len(meta.WritesArtifacts) != 2 || meta.WritesArtifacts[0].Path != "out/a.md" {
		t.Fatalf("writes_artifacts wrong: %v", meta.WritesArtifacts)
	}
	// Legacy []string form on the wire must decode with Track=true.
	for i, e := range meta.WritesArtifacts {
		if !e.Track {
			t.Errorf("legacy []string entry %d should default Track=true, got %+v", i, e)
		}
	}
	if meta.RunSourcePath != "enju/templates/demo" {
		t.Fatalf("run_source_path wrong: %q", meta.RunSourcePath)
	}
	if meta.Branch != "feature-x" {
		t.Fatalf("branch wrong: %q", meta.Branch)
	}
	if meta.RunParams["k"] != "v" {
		t.Fatalf("run_params wrong: %v", meta.RunParams)
	}
	if meta.InstanceParams["stem"] != "alpha" {
		t.Fatalf("instance_params wrong: %v", meta.InstanceParams)
	}
	if meta.Env["FOO"] != "bar" || meta.Env["BAZ"] != "qux" {
		t.Fatalf("env wrong: %v", meta.Env)
	}
	if meta.OutputsSchemaJSON == "" {
		t.Fatalf("expected outputs schema populated, got empty")
	}
}

// TestFetchTaskMetaServerError surfaces the error string verbatim.
func TestFetchTaskMetaServerError(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":"task not found"}`))
	}))
	defer ts.Close()

	c := newAPIClient(ts.URL)
	_, err := c.fetchTaskMeta(context.Background(), "nope")
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if err.Error() != "task not found" {
		t.Fatalf("expected passthrough error, got %q", err.Error())
	}
}

// TestFetchTaskMetaPartialPayload covers the defaults-applied path:
// minimal JSON still yields a usable taskMeta with zero values on the
// missing fields.
func TestFetchTaskMetaPartialPayload(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"state":"ready"}`))
	}))
	defer ts.Close()

	c := newAPIClient(ts.URL)
	meta, err := c.fetchTaskMeta(context.Background(), "minimal")
	if err != nil {
		t.Fatal(err)
	}
	if meta.ID != "minimal" {
		t.Fatalf("expected ID preserved, got %q", meta.ID)
	}
	if meta.State != "ready" {
		t.Fatalf("state not set: %q", meta.State)
	}
	if meta.ProjectID != 0 || meta.Citizens != 0 || len(meta.WritesArtifacts) != 0 {
		t.Fatalf("expected zero defaults, got %+v", meta)
	}
	if meta.Env != nil {
		t.Fatalf("empty env should stay nil, got %v", meta.Env)
	}
}

// TestFetchTaskMetaEmptyEnvStaysNil — the parser skips the env map
// assignment when every value is non-string or the map is empty. This
// guards against accidental {} differentiation that would break nil
// checks downstream.
func TestFetchTaskMetaEmptyEnvStaysNil(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"env": {}}`))
	}))
	defer ts.Close()

	c := newAPIClient(ts.URL)
	meta, err := c.fetchTaskMeta(context.Background(), "t")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Env != nil {
		t.Fatalf("expected nil env for empty map, got %v", meta.Env)
	}
}

// TestFetchTaskMetaMalformedJSON returns a wrapped parse error.
func TestFetchTaskMetaMalformedJSON(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{broken`))
	}))
	defer ts.Close()

	c := newAPIClient(ts.URL)
	_, err := c.fetchTaskMeta(context.Background(), "t")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// --- useFatClient ---

func TestUseFatClientNoWorkspaceReturnsFalse(t *testing.T) {
	c := &apiClient{} // no workspace
	meta := &taskMeta{ProjectID: 1, ProjectRemoteURL: "git@x.com:p.git"}
	if c.useFatClient(meta) {
		t.Fatal("expected false when workspace is nil")
	}
}

func TestUseFatClientNilMetaReturnsFalse(t *testing.T) {
	ws, err := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c := &apiClient{workspace: ws}
	if c.useFatClient(nil) {
		t.Fatal("expected false on nil meta")
	}
}

func TestUseFatClientWithRemoteURL(t *testing.T) {
	ws, err := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c := &apiClient{workspace: ws}
	meta := &taskMeta{ProjectID: 1, ProjectRemoteURL: "git@x.com:p.git"}
	if !c.useFatClient(meta) {
		t.Fatal("expected true when remote URL present")
	}
}

func TestUseFatClientWithExternalDir(t *testing.T) {
	ws, err := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	extDir := t.TempDir()
	ws.RegisterExternalDir(42, extDir)

	c := &apiClient{workspace: ws}
	meta := &taskMeta{ProjectID: 42} // no remote URL
	if !c.useFatClient(meta) {
		t.Fatal("expected true when external dir registered")
	}
}

func TestUseFatClientWithoutRemoteOrExternal(t *testing.T) {
	ws, err := mcpgit.NewWorkspace(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	c := &apiClient{workspace: ws}
	meta := &taskMeta{ProjectID: 99} // no remote, not registered
	if c.useFatClient(meta) {
		t.Fatal("expected false for self-hosted project without external dir")
	}
}
