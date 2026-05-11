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
	"github.com/enju-ai/enju/internal/testutil/gittest"
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
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	// Adopt a directory as project 7's local clone via the
	// post-Phase-A entry point: inspect the populated folder,
	// then EagerInitProjectClone runs git init + commit + wires
	// the managed bare.
	clone := t.TempDir()
	resultDir := "enju/runs/1-draft/draft"
	resultPath := filepath.Join(clone, resultDir, "result.md")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte("VERSION-1-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{WorkspaceRoot: ws.RootDir(), Logger: logger})
	target, terr := validateAndInspectPath(clone, false)
	if terr != nil {
		t.Fatalf("inspect: %v", terr)
	}
	if err := fc.EagerInitProjectClone(context.Background(), 7, clone, target); err != nil {
		t.Fatalf("EagerInitProjectClone: %v", err)
	}

	// Stage + commit the result file via a sibling enjugit
	// workspace pointing at the same root (avoids the coord-stub
	// dependency OpenWorkflow has via FetchProjectMetaExpanded).
	enjuWS, eerr := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
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
		{"", "enju/runs/1"},
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

// TestReadResultAtCommit_LazyClonesWhenMissing pins the webui-
// blind-spot fix: a project's clone may not exist in the
// reader's workspace (e.g. webui process for a bot-only project,
// where the operator never ran `enju mcp` to seed a workspace
// clone). When OpenExisting returns ErrCloneNotFound, the read
// path should fall back to ForProject(remoteURL) so the bare's
// objects become reachable, then return the file content.
func TestReadResultAtCommit_LazyClonesWhenMissing(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// 1) Bare remote, seeded with one initial commit so the
	// writer's ForProject can branch from refs/heads/main.
	bare := t.TempDir()
	gittest.InitBareWithSeed(t, bare)
	// Seed the bare via a writer enjugit workspace: ForProject(7, bare)
	// gives a Workflow whose clone is wired with origin=bare;
	// CommitArbitraryFiles + implicit push lands the commit there.
	writerRoot := t.TempDir()
	writerEnjugit, eerr := enjugit.NewWorkspace(writerRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if eerr != nil {
		t.Fatalf("writer enjugit: %v", eerr)
	}
	writerWF, err := writerEnjugit.ForProject(7, bare)
	if err != nil {
		t.Fatalf("writer enjugit.ForProject: %v", err)
	}
	resultDir := "enju/runs/1-draft/draft"
	// CommitArbitraryFiles takes WithLock internally via the
	// enjugit git layer; no project-level Lock needed.
	commitRes, err := writerWF.CommitArbitraryFiles(enjugit.CommitArbitraryFilesRequest{
		Files: []enjugit.FileWrite{
			{RepoRelPath: resultDir + "/result.md", Content: []byte("LAZY-CLONE-CONTENT")},
		},
		Subject:     "seed",
		AuthorName:  "Writer",
		AuthorEmail: "writer@example.com",
		Branch:      "main",
	})
	if err != nil {
		t.Fatalf("CommitArbitraryFiles: %v", err)
	}
	if commitRes == nil || commitRes.CommitSHA == "" {
		t.Fatal("empty commit SHA")
	}

	// 2) Coord stub returns the bare path as remote_url.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             7,
			"name":           "lazy-test",
			"remote_url":     bare,
			"default_branch": "main",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	// 3) Reader FatClient on a *fresh* workspace — no clone for
	// project 7 exists yet. OpenExisting will return
	// ErrCloneNotFound; the lazy-clone fallback should then
	// materialize the clone from the bare and read at commit.
	readerRoot := t.TempDir()
	readerWS, err := enjugit.NewWorkspace(readerRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if err != nil {
		t.Fatalf("reader Opener: %v", err)
	}
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:   srv.URL,
			Username:  "reader",
			AuthToken: "tok",
			Logger:    logger,
		}),
		WorkspaceRoot:   readerWS.RootDir(),
		Logger:    logger,
	})

	got, found, err := fc.ReadResultAtCommit(context.Background(), 7, commitRes.CommitSHA, resultDir)
	if err != nil {
		t.Fatalf("ReadResultAtCommit (lazy): %v", err)
	}
	if !found {
		t.Fatal("expected found=true after lazy clone")
	}
	if got != "LAZY-CLONE-CONTENT" {
		t.Errorf("content mismatch after lazy clone: got %q", got)
	}
}

// TestReadResultAtCommit_NoCloneNoRemoteIsQuiet covers the
// other arm: a project has no clone AND no remote_url (path-
// only project the reader has never been attached to). Lazy
// clone has no source to pull from; the read should return
// (false, nil) — same UX as "no submission yet".
func TestReadResultAtCommit_NoCloneNoRemoteIsQuiet(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/projects/7", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             7,
			"name":           "no-remote",
			"remote_url":     "",
			"default_branch": "main",
		})
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ws, err := enjugit.NewWorkspace(t.TempDir(), enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL:   srv.URL,
			Username:  "reader",
			AuthToken: "tok",
			Logger:    logger,
		}),
		WorkspaceRoot:   ws.RootDir(),
		Logger:    logger,
	})

	_, found, err := fc.ReadResultAtCommit(context.Background(), 7, "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef", "enju/runs/1-x/x")
	if err != nil {
		t.Errorf("expected nil error, got %v", err)
	}
	if found {
		t.Error("expected found=false when no clone and no remote_url")
	}
}

