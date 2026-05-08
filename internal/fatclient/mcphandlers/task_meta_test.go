package mcphandlers

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

func newAPIClient(baseURL string) *apiClient {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	coordClient := coord.New(coord.Config{
		BaseURL:  baseURL,
		Username: "tester",
		Logger:  logger,
	})
	return newClient(coordClient, "", logger)
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
	meta, err := c.fc.FetchTaskMeta(context.Background(), "t-1")
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
	_, err := c.fc.FetchTaskMeta(context.Background(), "nope")
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
	meta, err := c.fc.FetchTaskMeta(context.Background(), "minimal")
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
	meta, err := c.fc.FetchTaskMeta(context.Background(), "t")
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
	_, err := c.fc.FetchTaskMeta(context.Background(), "t")
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
}

// --- useFatClient ---

// newAPIClientWithWorkspace builds an apiClient bound to a real
// workspace + a service.FatClient that wraps it, so the useFatClient
// forwarder has somewhere to dispatch to. Tests below use this in
// place of the bare `newClient(nil, wsRoot, nil)` literal.
func newAPIClientWithWorkspace(wsRoot string) *apiClient {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return newClient(nil, wsRoot, logger)
}

func TestUseFatClientNoWorkspaceReturnsFalse(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := newClient(nil, "", logger)
	meta := &taskMeta{ProjectID: 1, ProjectRemoteURL: "git@x.com:p.git"}
	if c.fc.UseFatClient(meta) {
		t.Fatal("expected false when workspace is nil")
	}
}

func TestUseFatClientNilMetaReturnsFalse(t *testing.T) {
	ws, err := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	c := newAPIClientWithWorkspace(ws.RootDir())
	if c.fc.UseFatClient(nil) {
		t.Fatal("expected false on nil meta")
	}
}

func TestUseFatClientWithRemoteURL(t *testing.T) {
	ws, err := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	c := newAPIClientWithWorkspace(ws.RootDir())
	meta := &taskMeta{ProjectID: 1, ProjectRemoteURL: "git@x.com:p.git"}
	if !c.fc.UseFatClient(meta) {
		t.Fatal("expected true when remote URL present")
	}
}

func TestUseFatClientWithExternalDir(t *testing.T) {
	ws, err := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	extDir := t.TempDir()
	// Project paths come from the registry. Register via
	// projectreg + AttachRegistry; ForProject (and
	// HasExternalDir) will find the path the same way they do
	// in production.
	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{ID: 42, LocalPath: extDir}); err != nil {
		t.Fatal(err)
	}
	ws.AttachRegistry(reg)

	c := newAPIClientWithWorkspace(ws.RootDir())
	meta := &taskMeta{ProjectID: 42} // no remote URL
	if !c.fc.UseFatClient(meta) {
		t.Fatal("expected true when external dir registered")
	}
}

// TestUseFatClientWithoutRemoteOrExternal pins the post-Option-B
// behavior: a project with neither a remote URL nor an init-
// registered external dir STILL uses the fat-client path. The
// workspace creates a local clone on demand and commits land there.
//
// Pre-fix this returned false → submits silently went to the
// (broken) legacy POST path → vote/review/answer tasks recorded
// state=accepted with empty commit_sha and no on-disk directory.
// Only compute tasks worked because they bypass useFatClient.
func TestUseFatClientWithoutRemoteOrExternal(t *testing.T) {
	ws, err := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))))
	if err != nil {
		t.Fatal(err)
	}
	c := newAPIClientWithWorkspace(ws.RootDir())
	meta := &taskMeta{ProjectID: 99} // no remote, not registered
	if !c.fc.UseFatClient(meta) {
		t.Fatal("expected true: workspace exists, fat-client path commits to local clone even without a remote")
	}
}
