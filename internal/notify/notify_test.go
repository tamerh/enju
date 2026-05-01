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
// dispatch, no adapters. The local enju/events/live.jsonl file
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
	body, err := os.ReadFile(filepath.Join(dir, "enju", "events", "live.jsonl"))
	if err != nil {
		t.Fatalf("read live.jsonl: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 2 {
		t.Errorf("expected 2 lines, got %d: %q", len(lines), body)
	}

	// cursor.json should track last_seq=2.
	curBody, err := os.ReadFile(filepath.Join(dir, "enju", "events", "cursor.json"))
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
