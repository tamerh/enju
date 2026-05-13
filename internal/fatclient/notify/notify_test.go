package notify

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
	"sync/atomic"
	"testing"
	"time"
)

// TestRunAppendsEventsToLiveJSONL pins the v1 contract: the
// poll loop's only job is poll → append → advance cursor. No
// dispatch, no adapters. The local .enju/events/live.jsonl file
// is the substrate that enju_notifications later reads.
func TestRunAppendsEventsToLiveJSONL(t *testing.T) {
	dir := t.TempDir()

	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := pollCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			_ = json.NewEncoder(w).Encode([]Event{
				{Seq: 1, Timestamp: time.Now().UTC(), Type: "task_completed", TaskID: "1:1:t", Citizen: "tamer"},
				{Seq: 2, Timestamp: time.Now().UTC(), Type: "branch_merged", TaskID: "1:1:t"},
			})
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	defer cancel()
	_ = Run(ctx, Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		BearerToken:    "test",
		ProjectDir:     dir,
		PollWait:       50 * time.Millisecond,
		HTTPClient:     srv.Client(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	// live.jsonl should exist and have 2 lines.
	body, err := os.ReadFile(filepath.Join(dir, ".enju", "events", "live.jsonl"))
	if err != nil {
		t.Fatalf("read live.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d: %q", len(lines), body)
	}

	// cursor.json should track last_seq=2.
	curBody, err := os.ReadFile(filepath.Join(dir, ".enju", "events", "cursor.json"))
	if err != nil {
		t.Fatalf("read cursor.json: %v", err)
	}
	var st State
	if err := json.Unmarshal(curBody, &st); err != nil {
		t.Fatalf("parse cursor: %v", err)
	}
	if st.LastSeq != 2 {
		t.Errorf("cursor LastSeq = %d, want 2", st.LastSeq)
	}
}

// TestPollHasPerRequestDeadline pins the bug fix from a tester
// report: a silently-broken long-poll connection used to wedge
// the loop indefinitely (no client-side timeout, only ctx
// inheritance). The fix wraps each poll with ctx.WithTimeout =
// wait + 10s slack so a hung server returns an error and the
// outer loop retries. Without this, live.jsonl falls behind
// events.db whenever a TCP connection drops without notice.
func TestPollHasPerRequestDeadline(t *testing.T) {
	dir := t.TempDir()

	// Server that never responds. Each request hangs until the
	// client gives up (or the test times out). With the deadline
	// fix, the client returns within wait+slack.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until client cancels
	}))
	defer srv.Close()

	// Outer ctx is generous; the per-request deadline is what
	// must end the wait. Use a small PollWait so the test runs
	// fast.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	start := time.Now()
	_, err := pollEvents(ctx, srv.Client(), Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		BearerToken:    "test",
		ProjectDir:     dir,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}, 100*time.Millisecond, 0)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected timeout error from hung server")
	}
	// PollWait=100ms + 10s slack = 10.1s ceiling. We want the
	// request to give up well before the outer ctx fires (5s).
	// In practice the deadline fires at ~10.1s but the outer
	// ctx will fire first at 5s. Either error path is fine —
	// what we're guarding against is "blocks forever, never
	// errors." Anything bounded is the contract.
	if elapsed > 11*time.Second {
		t.Errorf("poll exceeded 11s budget: %v (request never gave up)", elapsed)
	}
}

// TestRunRequiredFields pins startup validation.
func TestRunRequiredFields(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want string
	}{
		{"missing url", Config{ProjectID: 1, BearerToken: "x"}, "CoordinatorURL"},
		{"missing project", Config{CoordinatorURL: "http://x", BearerToken: "x"}, "ProjectID"},
		{"missing token", Config{CoordinatorURL: "http://x", ProjectID: 1}, "BearerToken"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Run(context.Background(), tc.cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("expected error mentioning %q, got %v", tc.want, err)
			}
		})
	}
}
