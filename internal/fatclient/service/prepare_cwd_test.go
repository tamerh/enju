package service

// Tests for PrepareLLMClaimCWD's iter-branch fallback contract.
//
// The bot daemon calls PrepareLLMClaimCWD on every claim. For the
// FIRST iter of a task (iter-1), the coordinator has assigned a
// branch NAME (e.g. "1-foo/summarize/iter-1") but no git ref exists
// for it yet — the ref gets created lazily at submit time by
// prepareBranchForCommit. Pre-fix the wrapper called
// MaterializeRunRepo(iterBranch) directly, which errored with
// "branch X has no local or origin ref", the daemon logged a warn,
// and the handler ran with empty CWD — breaking `system_prompt:
// prompts/foo.md` and every other repo-relative lookup.
//
// Post-fix: PrepareLLMClaimCWD falls back to the run branch when
// the iter branch ref isn't created yet. The run branch is the
// fork base for iter-1 by definition, so the materialized tree
// matches what iter-1 starts from anyway.

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

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

func newProjectMetaServer(t *testing.T, projectID int64, defaultBranch string) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             projectID,
			"name":           "test-project",
			"remote_url":     "",
			"default_branch": defaultBranch,
		})
	})
	return httptest.NewServer(mux)
}

// TestPrepareLLMClaimCWD_IterBranchRefAbsent_FallsBackToRunBranch
// reproduces showcase_v13's "materialize iter-branch tree into
// claim CWD: branch '…' has no local or origin ref" error path.
//
// Setup mirrors production: a project clone exists with the run
// branch as the only ref. Coord has assigned an iter branch NAME
// for this claim but the ref isn't yet in the local store (it
// gets created at submit time, not at claim time).
func TestPrepareLLMClaimCWD_IterBranchRefAbsent_FallsBackToRunBranch(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	srv := newProjectMetaServer(t, 11, "main")
	defer srv.Close()

	wsRoot := t.TempDir()
	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg1 := projectreg.Open(regPath)
	projectPath1 := filepath.Join(wsRoot, "p1")
	if err := os.MkdirAll(projectPath1, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg1.Upsert(projectreg.Entry{ID: 11, LocalPath: projectPath1}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger), enjugit.WithRegistry(reg1))
	if err != nil {
		t.Fatal(err)
	}

	// Seed a clone with one commit on `main` so the run branch has
	// a real tip the materializer can walk. Use a sibling enjugit
	// Workspace pointing at the same root so we don't need the
	// fatclient init flow.
	wf, err := ws.ForProject(11, "")
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if _, err := wf.CommitArbitraryFiles(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{
			{RepoRelPath: "prompts/dev-bot2.md", Content: []byte("# Dev bot system prompt\n")},
			{RepoRelPath: "enju.yaml", Content: []byte("name: test\nversion: 1\n")},
		},
		Subject:     "seed",
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}

	fc := New(Config{
		WorkspaceRoot:   ws.RootDir(),
		ProjectRegistry: projectreg.Open(regPath),
		Coord: coord.New(coord.Config{
			BaseURL:   srv.URL,
			Username:  "dev-bot2",
			AuthToken: "test",
			Logger:    logger,
		}),
		Logger: logger,
	})

	// Coord-assigned iter branch name. No ref exists for it in the
	// local store — same shape as the bot's first iter-1 claim.
	iterBranch := "1-test/summarize/iter-1"
	runBranch := "main"

	path, err := fc.PrepareLLMClaimCWD(context.Background(),
		11, "dev-bot2", "11:1:summarize", 1, iterBranch, runBranch)
	if err != nil {
		t.Fatalf("PrepareLLMClaimCWD with absent iter branch ref: %v", err)
	}
	if path == "" {
		t.Fatal("expected materialized CWD path, got empty (handler would run with no CWD and system_prompt would fail)")
	}

	// Post-Phase-8 layout: bot scratch lives under the project's
	// .enju/ tree, not under a machine-wide ~/.enju/workspaces/.
	// Pin the shape so a future revert doesn't silently regress to
	// the machine-scoped path (which broke single-machine no-origin
	// projects on showcase_v14).
	wantPrefix := "/.enju/bots/dev-bot2/scratch/"
	if !strings.Contains(path, wantPrefix) {
		t.Errorf("scratch path %q should contain %q (project-scoped layout)", path, wantPrefix)
	}

	// The materialized tree must contain the seeded files —
	// proves the fallback actually walked the run branch.
	if _, err := os.Stat(filepath.Join(path, "prompts/dev-bot2.md")); err != nil {
		t.Errorf("prompts/dev-bot2.md not materialized in CWD %q: %v", path, err)
	}
	if _, err := os.Stat(filepath.Join(path, "enju.yaml")); err != nil {
		t.Errorf("enju.yaml not materialized in CWD %q: %v", path, err)
	}
}
