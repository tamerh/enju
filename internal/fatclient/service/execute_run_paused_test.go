package service

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

// TestExecuteRun_RefusesPausedRun is the bug hunt B-2 regression.
// PAUSED is the operator's circuit-breaker; the coord-side
// claim/submit gating that would enforce it is an acknowledged
// deferred gap, so enju_execute_run used to blow straight
// through a pause, drive every task to ACCEPTED, and leave the
// run stuck at "paused 100%" until a manual resume. ExecuteRun
// must now refuse a paused run up front with an actionable error
// (and NOT a misleading "no ready compute" / git error).
func TestExecuteRun_RefusesPausedRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Minimal run payload — only branch+state are read by
		// the ExecuteRun pre-flight, which short-circuits on
		// paused before any /ready or git work.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":1,"project_id":12,"seq":7,"state":"paused","branch":"pause-test"}`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger,
		}),
		// Non-empty WorkspaceRoot so s.enjugit is wired (ExecuteRun
		// requires a local workspace before the pre-flight).
		WorkspaceRoot: t.TempDir(),
		Logger:        logger,
	})

	res, err := fc.ExecuteRun(context.Background(), ExecuteRunParams{
		ProjectID: 12, RunSeq: 7, MaxTasks: 10,
	})
	if err == nil {
		t.Fatalf("B-2 regression: ExecuteRun drove a paused run instead of refusing (result=%+v)", res)
	}
	if !strings.Contains(err.Error(), "paused") {
		t.Errorf("error should explain the run is paused, got: %v", err)
	}
	if !strings.Contains(err.Error(), "enju_resume_run") {
		t.Errorf("error should point at enju_resume_run as the recovery, got: %v", err)
	}
}

// TestExecuteRun_AllowsNonPausedPreflight pins that the new gate
// is scoped to paused only — an active run passes the pre-flight
// (it then fails later for unrelated reasons in this stub, which
// is fine; the assertion is only that the error is NOT the
// paused refusal).
func TestExecuteRun_AllowsNonPausedPreflight(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(r.URL.Path, "/runs/") && !strings.Contains(r.URL.RawQuery, "ready") && strings.HasSuffix(r.URL.Path, "/7") {
			_, _ = w.Write([]byte(`{"id":1,"project_id":12,"seq":7,"state":"active","branch":"main"}`))
			return
		}
		// /ready and anything else: empty list → run reads idle.
		_, _ = w.Write([]byte(`[]`))
	}))
	defer srv.Close()

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := New(Config{
		Coord: coord.New(coord.Config{
			BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger,
		}),
		WorkspaceRoot: t.TempDir(),
		Logger:        logger,
	})

	_, err := fc.ExecuteRun(context.Background(), ExecuteRunParams{
		ProjectID: 12, RunSeq: 7, MaxTasks: 1,
	})
	if err != nil && strings.Contains(err.Error(), "paused") {
		t.Errorf("active run must NOT hit the paused gate, got: %v", err)
	}
}
