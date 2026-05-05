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
	"github.com/enju-ai/enju/internal/fatclient/workspace"
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
	ws, err := workspace.NewWorkspace(wsRoot, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Adopt a directory as project 7's local clone. The
	// InitDirAsProject helper does the git init + first commit
	// and wires it through the workspace.
	clone := t.TempDir()
	resultDir := "enju/runs/1-draft/draft"
	resultPath := filepath.Join(clone, resultDir, "result.md")
	if err := os.MkdirAll(filepath.Dir(resultPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(resultPath, []byte("VERSION-1-CONTENT"), 0o644); err != nil {
		t.Fatal(err)
	}

	fc := New(Config{Workspace: ws, Logger: logger})
	if _, err := fc.InitDirAsProject(clone); err != nil {
		t.Fatalf("InitDirAsProject: %v", err)
	}
	if err := fc.RegisterAdoptedDir(7, clone); err != nil {
		t.Fatalf("RegisterAdoptedDir: %v", err)
	}

	// Stage + commit the result file via the workspace's git.
	proj, perr := ws.ForProject(7, "")
	if perr != nil {
		t.Fatalf("ForProject: %v", perr)
	}
	commitRes, err := proj.CommitFiles(workspace.CommitFilesRequest{
		Files: []workspace.FileWrite{
			{RepoRelPath: resultDir + "/result.md", Content: []byte("VERSION-1-CONTENT")},
		},
		CommitMsg:   "seed",
		AuthorName:  "Test",
		AuthorEmail: "test@example.com",
	})
	if err != nil {
		t.Fatalf("CommitFiles: %v", err)
	}
	if commitRes == nil || commitRes.CommitSHA == "" {
		t.Fatal("CommitFiles returned empty SHA")
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
