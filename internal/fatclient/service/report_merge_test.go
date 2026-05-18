package service

// Tests for FatClient.reportMerge — the post-FF-push audit POST
// that ALSO drives the SUBMITTED→ACCEPTED transition coord-side
// (see internal/coordinator/service/report_merge.go).
//
// The bug this guards against: pre-fix, reportMerge was pure
// fire-and-forget; a transient coord error (SQLITE_BUSY,
// network blip, 503 under peak parallel-merge concurrency)
// silently dropped the audit POST AND the state flip, leaving
// the task wedged in SUBMITTED with the merge already in origin.
// Load testing (500-task fan-out) caught a small number of tasks
// stuck this way. Fix: bounded retry with exponential backoff +
// bubble final error up through ExecuteOutcome on exhaustion.
//
// Idempotency: the /merges handler emits branch_merged inside
// the SUBMITTED state guard and no-ops both the audit event and
// the state flip when the task is already past SUBMITTED. So a
// duplicate POST after the first landed call is safe — see
// internal/coordinator/service/report_merge.go.

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/coord"
)

func newReportMergeClient(t *testing.T, baseURL string) *FatClient {
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

// TestReportMerge_RetriesOn503ThenSucceeds is the regression
// guard for the silent-stall bug. The mock coord fails the
// first /merges POST with 503 (the canonical "transient load"
// envelope) then succeeds on the second. We assert:
// - reportMerge returns nil (the retry recovered)
// - exactly two requests landed at /merges (one fail, one
// success retry — proves the retry actually fired)
// - on the unfixed code path (no retry), the same harness
// would see one request and a non-nil error.
func TestReportMerge_RetriesOn503ThenSucceeds(t *testing.T) {
	// Shrink the backoff schedule for a fast test. Keep the
	// shape (one retry attempt is all we need for the success
	// path) but trim the sleep to milliseconds so the suite
	// stays fast.
	saved := reportMergeRetryBackoffs
	reportMergeRetryBackoffs = []time.Duration{
		5 * time.Millisecond,
		10 * time.Millisecond,
		20 * time.Millisecond,
		40 * time.Millisecond,
		80 * time.Millisecond,
	}
	defer func() { reportMergeRetryBackoffs = saved }()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := atomic.AddInt32(&hits, 1)
		if r.URL.Path != "/api/v1/projects/4/runs/6/merges" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		if n == 1 {
			// First attempt: 503, simulating coord overloaded
			// under peak parallel-merge concurrency.
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = w.Write([]byte(`{"error": "coord overloaded"}`))
			return
		}
		// Subsequent attempts: 200. The retry recovered.
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status": "recorded"}`))
	}))
	defer srv.Close()

	fc := newReportMergeClient(t, srv.URL)
	_, err := fc.reportMerge(context.Background(), 4, 6,
		"4:1:i42:s4", "topic/run-6/4:1:i42:s4",
		"run-6", "deadbeefdeadbeefdeadbeefdeadbeefdeadbeef")
	if err != nil {
		t.Fatalf("reportMerge after retry must succeed, got: %v", err)
	}
	if got := atomic.LoadInt32(&hits); got != 2 {
		t.Errorf("expected exactly 2 POSTs (1 fail + 1 retry), got %d", got)
	}
}

// TestReportMerge_ExhaustsRetriesReturnsError is the loud-failure
// guard. When every retry fails, reportMerge MUST surface the
// error so applyAcceptedMerges can bubble it through
// ExecuteOutcome. Silent stall is the failure mode this guards
// against.
func TestReportMerge_ExhaustsRetriesReturnsError(t *testing.T) {
	saved := reportMergeRetryBackoffs
	reportMergeRetryBackoffs = []time.Duration{
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
		1 * time.Millisecond,
	}
	defer func() { reportMergeRetryBackoffs = saved }()

	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error": "still down"}`))
	}))
	defer srv.Close()

	fc := newReportMergeClient(t, srv.URL)
	_, err := fc.reportMerge(context.Background(), 4, 6,
		"4:1:i42:s4", "topic/x", "run-6",
		"cafef00dcafef00dcafef00dcafef00dcafef00d")
	if err == nil {
		t.Fatal("expected error after exhausting retries")
	}
	expected := int32(len(reportMergeRetryBackoffs) + 1)
	if got := atomic.LoadInt32(&hits); got != expected {
		t.Errorf("expected %d total attempts, got %d", expected, got)
	}
}

// TestReportMerge_CtxCancellationBreaksOut confirms a cancelled
// context short-circuits the retry-backoff sleep cleanly. The
// fat-client's submit pipeline runs under a parent context; if
// that context is cancelled mid-stage, reportMerge must not
// hang for the full backoff schedule.
func TestReportMerge_CtxCancellationBreaksOut(t *testing.T) {
	saved := reportMergeRetryBackoffs
	reportMergeRetryBackoffs = []time.Duration{
		2 * time.Second, // long enough to detect a hang
		2 * time.Second,
		2 * time.Second,
		2 * time.Second,
		2 * time.Second,
	}
	defer func() { reportMergeRetryBackoffs = saved }()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error": "still down"}`))
	}))
	defer srv.Close()

	fc := newReportMergeClient(t, srv.URL)
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel after first failed POST has had time to schedule
	// its backoff sleep. The select on ctx.Done must wake.
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()

	start := time.Now()
	_, err := fc.reportMerge(ctx, 4, 6, "4:1:i42:s4", "topic/x", "run-6", "deadbeef")
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected error on cancelled ctx")
	}
	// Generous bound: should be well under one full 2s backoff.
	if elapsed > 1500*time.Millisecond {
		t.Errorf("ctx cancel did not break out of backoff promptly (elapsed=%v)", elapsed)
	}
}
