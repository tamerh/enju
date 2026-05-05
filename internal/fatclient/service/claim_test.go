package service

// Tests for FatClient.ClaimTask covering the envelope-check
// contract: a coord refusal (4xx with `{"error": "..."}` body)
// must surface as a real Go error from ClaimTask, not as a
// successful ClaimResult wrapping the error envelope.
//
// The production symptom this guards against: bot daemon
// scoped to project 3 received a (mis-routed) task ID from
// project 1, POSTed claim, coord returned 403 "not a member",
// pre-fix ClaimTask wrapped the body and returned no error,
// daemon set activeClaim and proceeded to FetchTaskMeta which
// THEN surfaced the membership error. By that point the bot
// had a phantom claim coord-side that only the reaper could
// time out.

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

func newClaimClient(t *testing.T, baseURL string) *FatClient {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	c := coord.New(coord.Config{
		BaseURL:   baseURL,
		Username:  "bot1",
		AuthToken: "t",
		Logger:    logger,
	})
	return New(Config{Coord: c, Logger: logger})
}

func TestClaimTask_SurfacesNotAMember(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error": "not a member of project 1"}`))
	}))
	defer srv.Close()

	fc := newClaimClient(t, srv.URL)
	res, err := fc.ClaimTask(context.Background(), ClaimParams{TaskID: "1:1:draft"})
	if err == nil {
		t.Fatalf("expected error from 403 response, got result %+v", res)
	}
	if !strings.Contains(err.Error(), "not a member") {
		t.Errorf("error should carry coord message verbatim, got: %v", err)
	}
	if res != nil {
		t.Errorf("ClaimResult must be nil on refusal so the daemon doesn't set activeClaim, got: %+v", res)
	}
}

func TestClaimTask_SurfacesAlreadyClaimed(t *testing.T) {
	// The other common 4xx envelope: a race where another
	// citizen got there first. Same shape — must surface as
	// error, not phantom-success.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusConflict)
		_, _ = w.Write([]byte(`{"error": "task already claimed by alice"}`))
	}))
	defer srv.Close()

	fc := newClaimClient(t, srv.URL)
	_, err := fc.ClaimTask(context.Background(), ClaimParams{TaskID: "1:1:t"})
	if err == nil {
		t.Fatal("expected error on already-claimed envelope")
	}
	if !strings.Contains(err.Error(), "already claimed") {
		t.Errorf("error message should pass through: %v", err)
	}
}
