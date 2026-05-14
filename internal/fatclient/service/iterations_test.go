package service

// Tests for ListTaskIterations + ReadResultAtCommit. Covers
// the happy-path coord round-trip, the (found=false) miss path
// for both methods, and the timestamp-decode contract on the
// shared wire.Iteration type.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

func TestListTaskIterations(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/tasks/7:1:draft/iterations", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{
				"seq":             1,
				"citizen":         "tamer",
				"outcome":         "rejected",
				"claimed_at":      "2026-05-04T10:00:00Z",
				"submitted_at":    "2026-05-04T11:00:00Z",
				"commit_sha":      "abc123",
				"branch":          "feature/x",
				"review_decision": "request_changes",
			},
			{
				"seq":        2,
				"citizen":    "tamer",
				"outcome":    "active",
				"claimed_at": "2026-05-04T12:00:00Z",
			},
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:   srv.URL,
			Username:  "tamer",
			AuthToken: "test-token",
			Logger:    logger,
		}),
		Logger: logger,
	})

	got, err := fc.ListTaskIterations(context.Background(), "7:1:draft")
	if err != nil {
		t.Fatalf("ListTaskIterations: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("want 2 iterations, got %d", len(got))
	}
	if got[0].Seq != 1 || got[0].Outcome != "rejected" || got[0].ReviewDecision != "request_changes" {
		t.Errorf("iter[0] mismatch: %+v", got[0])
	}
	if got[0].ClaimedAt.IsZero() {
		t.Errorf("iter[0].ClaimedAt didn't decode: %v", got[0].ClaimedAt)
	}
	if got[0].SubmittedAt == nil || got[0].SubmittedAt.IsZero() {
		t.Errorf("iter[0].SubmittedAt should be set, got %v", got[0].SubmittedAt)
	}
	// Active iteration: SubmittedAt absent on the wire,
	// pointer should stay nil after decode.
	if got[1].SubmittedAt != nil {
		t.Errorf("iter[1] is active; SubmittedAt should be nil, got %v", got[1].SubmittedAt)
	}
	if got[1].Outcome != "active" {
		t.Errorf("iter[1].Outcome mismatch: %q", got[1].Outcome)
	}
}

func TestListTaskIterations_EmptyTaskID(t *testing.T) {
	fc := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if _, err := fc.ListTaskIterations(context.Background(), ""); err == nil {
		t.Errorf("expected error for empty task_id")
	}
}

func TestReadResultAtCommit(t *testing.T) {
	// Build a tiny real git repo with a known commit, hand
	// the FatClient a workspace pointing at it, and verify the
	// read returns the file contents at that commit.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wsRoot := t.TempDir()

	// Single shared registry: FatClient.EagerInitProjectClone
	// upserts into it, and the sibling enjugit workspace we open
	// for the seed commit reads via the same handle. Without this
	// shared registry, the sibling workspace can't resolve project
	// 7 post-NDW.2.
	regPath := filepath.Join(t.TempDir(), "projects.json")
	reg := projectreg.Open(regPath)

	// Adopt a directory as project 7's local clone via the
	// post-Phase-A entry point: inspect the populated folder,
	// then EagerInitProjectClone runs git init + commit + wires
	// the registry entry.
	clone := t.TempDir()
	resultDir := ".enju/runs/1-draft/draft"
	resultPath := filepath.Join(clone, resultDir, "result.md")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte("VERSION-1-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{WorkspaceRoot: wsRoot, ProjectRegistry: reg, Logger: logger})
	target, terr := validateAndInspectPath(clone, false, nil)
	if terr != nil {
		t.Fatalf("inspect: %v", terr)
	}
	if err := fc.EagerInitProjectClone(context.Background(), 7, clone, target); err != nil {
		t.Fatalf("EagerInitProjectClone: %v", err)
	}

	// Stage + commit the result file via a sibling enjugit
	// workspace pointing at the same root (avoids the coord-stub
	// dependency OpenWorkflow has via FetchProjectMetaExpanded).
	// Re-open the same registry file so project 7 resolves.
	enjuWS, eerr := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(logger), enjugit.WithRegistry(projectreg.Open(regPath)))
	if eerr != nil {
		t.Fatalf("enjugit Workspace: %v", eerr)
	}
	wf, ferr := enjuWS.ForProject(7, "")
	if ferr != nil {
		t.Fatalf("enjugit ForProject: %v", ferr)
	}
	commitRes, err := wf.CommitArbitraryFiles(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{
			{RepoRelPath: resultDir + "/result.md", Content: []byte("VERSION-1-CONTENT")},
		},
		Subject:     "seed",
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("CommitArbitraryFiles: %v", err)
	}
	if commitRes == nil || commitRes.CommitSHA == "" {
		t.Fatal("CommitArbitraryFiles returned empty SHA")
	}
	commitSHA := commitRes.CommitSHA

	// Hit: read at the commit we just made.
	got, found, err := fc.ReadResultAtCommit(context.Background(), 7, commitSHA, resultDir)
	if err != nil {
		t.Fatalf("ReadResultAtCommit: %v", err)
	}
	if !found {
		t.Fatalf("expected found=true at known commit")
	}
	if got != "VERSION-1-CONTENT" {
		t.Errorf("content mismatch: got %q", got)
	}

	// Miss: same commit, wrong dir → found=false, no error.
	_, found, err = fc.ReadResultAtCommit(context.Background(), 7, commitSHA, "nonexistent/dir")
	if err != nil {
		t.Errorf("expected nil error on miss, got %v", err)
	}
	if found {
		t.Errorf("expected found=false for missing path")
	}
}

func TestReadResultAtCommit_EmptyArgs(t *testing.T) {
	// Empty commit or dir → quiet (false, nil). Don't open
	// the project clone for trivially-empty input.
	fc := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	for _, c := range []struct {
		commit, dir string
	}{
		{"", ".enju/runs/1"},
		{"abc", ""},
		{"", ""},
	} {
		_, found, err := fc.ReadResultAtCommit(context.Background(), 7, c.commit, c.dir)
		if err != nil {
			t.Errorf("commit=%q dir=%q: unexpected error %v", c.commit, c.dir, err)
		}
		if found {
			t.Errorf("commit=%q dir=%q: expected found=false", c.commit, c.dir)
		}
	}
}

// silence unused-import warning for time when only struct
// fields reference it transitively (the iteration test above
// reads .ClaimedAt as a time.Time).
var _ = time.Time{}

// TestReadResultAtCommit_UnregisteredProjectIsQuiet pins the
// post-NDW.2 read-only-surface contract: a project that isn't
// registered on this machine (the webui-blind-spot scenario:
// browsing a project the user hasn't adopted locally) returns
// (false, nil) — no submission viewable here, but not an error.
//
// Replaces the previous TestReadResultAtCommit_LazyClonesWhenMissing
// which pinned the now-removed silent-lazy-clone fallback.
// Adoption goes through enju_create_project; ReadResultAtCommit
// never materializes a clone on its own.
func TestReadResultAtCommit_UnregisteredProjectIsQuiet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	ws, err := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(),
		enjugit.WithLogger(logger), enjugit.WithRegistry(reg))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:   "http://example.invalid",
			Username:  "reader",
			AuthToken: "tok",
			Logger:    logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		ProjectRegistry: reg,
		Logger:          logger,
	})

	_, found, err := fc.ReadResultAtCommit(context.Background(), 7,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ".enju/runs/1-x/x")
	if err != nil {
		t.Errorf("expected nil error for unregistered project, got %v", err)
	}
	if found {
		t.Error("expected found=false when project not registered")
	}
}

// TestReadResultAtCommit_RegisteredButNoCloneIsQuiet covers the
// arm where the project IS registered but the .git hasn't been
// materialized yet. Mirrors the unregistered case — quiet
// (false, nil) — so the webui's "no submission yet" banner
// renders without surfacing a crash.
func TestReadResultAtCommit_RegisteredButNoCloneIsQuiet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	wsRoot := t.TempDir()
	projectPath := filepath.Join(wsRoot, "adopted")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	if err := reg.Upsert(projectreg.Entry{ID: 7, LocalPath: projectPath}); err != nil {
		t.Fatal(err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(),
		enjugit.WithLogger(logger), enjugit.WithRegistry(reg))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:   "http://example.invalid",
			Username:  "reader",
			AuthToken: "tok",
			Logger:    logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		ProjectRegistry: reg,
		Logger:          logger,
	})

	_, found, err := fc.ReadResultAtCommit(context.Background(), 7,
		"deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", ".enju/runs/1-x/x")
	if err != nil {
		t.Errorf("expected nil error for missing clone, got %v", err)
	}
	if found {
		t.Error("expected found=false when registered but no clone")
	}
}

