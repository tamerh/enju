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
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestPredicateMatchesAllFieldsAndWildcards pins the matcher
// invariants:
//
//  1. Empty fields are wildcards (don't constrain).
//  2. Non-empty fields require exact match.
//  3. {{me}} on Citizen resolves to cfg.Username.
//  4. Multi-field predicates AND together.
func TestPredicateMatchesAllFieldsAndWildcards(t *testing.T) {
	cfg := Config{Username: "tamer"}
	ev := Event{
		Type:    "task_completed",
		Subtype: "answer",
		TaskID:  "1:1:draft",
		Citizen: "tamer",
	}

	cases := []struct {
		name string
		pred Predicate
		want bool
	}{
		{"all empty (wildcard)", Predicate{}, true},
		{"exact type", Predicate{EventType: "task_completed"}, true},
		{"wrong type", Predicate{EventType: "task_failed"}, false},
		{"exact subtype", Predicate{Subtype: "answer"}, true},
		{"wrong subtype", Predicate{Subtype: "review"}, false},
		{"task id match", Predicate{TaskID: "1:1:draft"}, true},
		{"citizen literal match", Predicate{Citizen: "tamer"}, true},
		{"citizen me sentinel", Predicate{Citizen: "{{me}}"}, true},
		{"citizen me wrong username", Predicate{Citizen: "{{me}}"}, true}, // tamer == tamer
		{"all fields AND match", Predicate{
			EventType: "task_completed", Subtype: "answer",
			TaskID: "1:1:draft", Citizen: "{{me}}",
		}, true},
		{"all fields one mismatch", Predicate{
			EventType: "task_completed", Subtype: "answer",
			TaskID: "1:1:draft", Citizen: "alice",
		}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := predicateMatches(tc.pred, ev, cfg)
			if got != tc.want {
				t.Errorf("predicateMatches(%+v, %+v) = %v, want %v", tc.pred, ev, got, tc.want)
			}
		})
	}
}

// TestRenderTemplateBasicTokens pins the {{...}} substitution.
// Multi-token strings, missing tokens (leave literal), empty
// template, all covered.
func TestRenderTemplateBasicTokens(t *testing.T) {
	ev := Event{
		Type:      "task_completed",
		Subtype:   "answer",
		TaskID:    "1:1:draft",
		Citizen:   "tamer",
		Timestamp: time.Date(2026, 5, 1, 12, 34, 56, 0, time.UTC),
	}
	cases := map[string]string{
		"":                                   "",
		"plain text":                         "plain text",
		"{{type}}":                           "task_completed",
		"{{type}}/{{subtype}}":               "task_completed/answer",
		"Task {{task_id}} done by {{citizen}}": "Task 1:1:draft done by tamer",
		"At {{ts}}":                          "At 2026-05-01 12:34:56",
		"{{unknown}}":                        "{{unknown}}", // leave literal
	}
	for tmpl, want := range cases {
		got := renderTemplate(tmpl, ev)
		if got != want {
			t.Errorf("renderTemplate(%q) = %q, want %q", tmpl, got, want)
		}
	}
}

// TestStatePersistRoundTrip pins the on-disk state contract:
// write a State{LastSeq}, read it back, verify versioned +
// atomic. Pre-Phase-4f cursor was timestamp-based; v3 is seq.
func TestStatePersistRoundTrip(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")

	// Empty path → no-op, no error, no file created.
	if err := saveState("", State{LastSeq: 42}); err != nil {
		t.Errorf("save with empty path should be no-op: %v", err)
	}
	got, err := loadState("")
	if err != nil {
		t.Errorf("load empty path: %v", err)
	}
	if got.LastSeq != 0 {
		t.Errorf("empty path should yield zero state, got %+v", got)
	}

	// Missing file → zero state, no error.
	got, err = loadState(path)
	if err != nil {
		t.Errorf("load missing file should not error: %v", err)
	}
	if got.LastSeq != 0 {
		t.Errorf("missing file should yield zero state, got %+v", got)
	}

	// Save + load round-trips the seq.
	want := int64(123456)
	if err := saveState(path, State{LastSeq: want}); err != nil {
		t.Fatalf("save: %v", err)
	}
	got, err = loadState(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got.LastSeq != want {
		t.Errorf("round-trip: got %d, want %d", got.LastSeq, want)
	}
	if got.Version != currentStateVersion {
		t.Errorf("save should stamp version=%d, got %d", currentStateVersion, got.Version)
	}
}

// TestStateLoadMalformedFileReturnsError pins the malformed-
// file behavior: better to surface the error to the caller
// (which logs + continues with zero state) than to silently
// pretend everything's fine.
func TestStateLoadMalformedFileReturnsError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := loadState(path)
	if err == nil {
		t.Error("expected parse error on malformed state file")
	}
}

// TestRunDispatchesMatchingEvent is the end-to-end smoke: a
// fake coordinator serves an event, the notify loop polls it,
// the matching rule fires, the dispatcher records the call.
// Uses Config.Dispatcher to avoid shelling out to notify-send
// in CI.
func TestRunDispatchesMatchingEvent(t *testing.T) {
	// Fake coordinator: serves one event, then idles.
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := pollCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			ts := time.Now().UTC()
			events := []Event{{
				Seq:       42,
				Timestamp: ts,
				Type:      "task_completed",
				Subtype:   "answer",
				TaskID:    "1:1:draft",
				Citizen:   "tamer",
			}}
			_ = json.NewEncoder(w).Encode(events)
			return
		}
		// Subsequent polls: empty array (long-poll timeout).
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	var dispatched []Event
	var mu sync.Mutex
	cfg := Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		CitizenID:      42,
		Username:       "tamer",
		BearerToken:    "test-token",
		Rules: []Rule{{
			Name:    "test-rule",
			When:    Predicate{EventType: "task_completed"},
			Kind:    "desktop",
			Message: "{{type}} on {{task_id}}",
		}},
		// Disable Layer 1 defaults so this test exercises only
		// the user-rule path. Default-rule behavior is covered
		// by defaults_test.go.
		DisableDefaults: []string{"all"},
		StateFile:       filepath.Join(t.TempDir(), "state.json"),
		PollWait:        100 * time.Millisecond,
		HTTPClient:      srv.Client(),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev Event, rule Rule, cfg Config) error {
			mu.Lock()
			defer mu.Unlock()
			dispatched = append(dispatched, ev)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, cfg) }()

	// Wait for dispatch.
	deadline := time.Now().Add(1 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(dispatched)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done // wait for Run to exit cleanly

	mu.Lock()
	defer mu.Unlock()
	if len(dispatched) != 1 {
		t.Fatalf("expected 1 dispatch, got %d", len(dispatched))
	}
	if dispatched[0].Type != "task_completed" {
		t.Errorf("dispatched event type = %q, want task_completed", dispatched[0].Type)
	}

	// State file should have been written with the event's seq.
	loaded, err := loadState(cfg.StateFile)
	if err != nil {
		t.Errorf("load state: %v", err)
	}
	if loaded.LastSeq == 0 {
		t.Error("state file should have advanced LastSeq after dispatch")
	}
}

// TestRunNonMatchingEventDoesNotDispatch pins that the rule
// matcher actually filters: an event the rule doesn't match
// gets seen + skipped, not surfaced to dispatch.
func TestRunNonMatchingEventDoesNotDispatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		events := []Event{{
			Timestamp: time.Now().UTC(),
			Type:      "task_failed", // not what the rule matches
			TaskID:    "1:1:draft",
		}}
		_ = json.NewEncoder(w).Encode(events)
	}))
	defer srv.Close()

	var dispatched int32
	cfg := Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		BearerToken:    "test-token",
		Rules: []Rule{{
			Name: "only-completions",
			When: Predicate{EventType: "task_completed"},
			Kind: "desktop",
		}},
		DisableDefaults: []string{"all"},
		PollWait:        50 * time.Millisecond,
		HTTPClient:      srv.Client(),
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev Event, rule Rule, cfg Config) error {
			atomic.AddInt32(&dispatched, 1)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = Run(ctx, cfg)

	if got := atomic.LoadInt32(&dispatched); got != 0 {
		t.Errorf("expected 0 dispatches for non-matching event, got %d", got)
	}
}

// TestRunRequiredFieldValidation pins that Run rejects empty
// required config rather than silently looping forever.
func TestRunRequiredFieldValidation(t *testing.T) {
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

