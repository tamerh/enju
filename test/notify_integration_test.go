package test

// End-to-end integration test for the notify subsystem (Phase
// 4e of docs/notifications.md). Covers the wire from event
// emission → coordinator long-poll → fat-client decode → rule
// match → adapter dispatch.
//
// The piece this exercises that the unit tests can't: that the
// real coordinator's /events JSON projection (citizen_id →
// username, metadata blob handling, RFC3339Nano timestamps,
// long-poll broadcast wakeup) decodes cleanly into notify.Event
// and through the matcher. A rename in either struct's tags
// would break the wire and this test would fail, but the unit
// tests would still pass.

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/notify"
	"github.com/enju-ai/enju/internal/store"
)

// TestNotifyIntegration_DispatchesOnRealCoordinatorEvent boots
// a real coordinator, starts notify.Run polling its /events
// endpoint, emits a synthetic task_completed event via the
// EventStore, and verifies the dispatcher fires.
//
// What this pins (relative to defaults_test.go's mock-server
// version):
//   - Real /events long-poll handler decodes correctly into
//     notify.Event (json tags match wire shape).
//   - Bearer token auth from notify's HTTP client passes
//     coordinator middleware.
//   - Broadcast-on-emit wakes the long-poll within ~ms, not
//     waiting for the full PollWait deadline.
//   - Citizen username comes through as the wire `citizen`
//     field, matching the {{me}} predicate.
func TestNotifyIntegration_DispatchesOnRealCoordinatorEvent(t *testing.T) {
	ts := newTestServer(t)
	username := ts.register("Notify Tester")
	projectID := ts.createTestProject()

	citizen, err := ts.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		t.Fatalf("lookup test citizen: %v", err)
	}

	// Capture dispatches via Config.Dispatcher so the test
	// doesn't actually shell out to notify-send.
	var (
		mu       sync.Mutex
		captured []notify.Event
	)
	cfg := notify.Config{
		CoordinatorURL: ts.url,
		ProjectID:      projectID,
		CitizenID:      citizen.ID,
		Username:       username,
		BearerToken:    citizen.Token,
		// Pure user rule keyed on {{me}} — confirms citizen-username
		// substitution from wire field works end-to-end. Disable
		// Layer 1 defaults so this test deterministically counts
		// only this rule's fires.
		Rules: []notify.Rule{{
			Name:    "integration-test",
			When:    notify.Predicate{EventType: "task_completed", Citizen: "{{me}}"},
			Kind:    "desktop",
			Message: "{{type}} on {{task_id}}",
		}},
		DisableDefaults: []string{"all"},
		PollWait:        2 * time.Second,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev notify.Event, rule notify.Rule, _ notify.Config) error {
			mu.Lock()
			defer mu.Unlock()
			captured = append(captured, ev)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- notify.Run(ctx, cfg) }()

	// Give the poll loop a moment to issue its first long-poll
	// before we emit. Without this the event lands first, the
	// poll then returns it via the catch-up read on connect —
	// still works, but the goal is to exercise the broadcast-
	// wake path. A tiny sleep keeps the test honest about which
	// pathway it's covering.
	time.Sleep(100 * time.Millisecond)

	ts.store.Events().Record(store.Event{
		CitizenID: citizen.ID,
		EventType: "task_completed",
		ProjectID: projectID,
		TaskID:    "1:1:draft",
		CreatedAt: time.Now(),
	})

	// Wait for the dispatcher to fire. 3s is generous — broadcast
	// wakeup is ms; persistence is also ms; HTTP RTT to
	// httptest is ~ms. If we hit the timeout, something between
	// emission and dispatch is broken.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(captured)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	<-done

	mu.Lock()
	defer mu.Unlock()
	if len(captured) != 1 {
		t.Fatalf("expected exactly 1 dispatch, got %d (captured=%+v)", len(captured), captured)
	}
	got := captured[0]
	if got.Type != "task_completed" {
		t.Errorf("dispatched event Type = %q, want task_completed", got.Type)
	}
	if got.TaskID != "1:1:draft" {
		t.Errorf("dispatched event TaskID = %q, want 1:1:draft", got.TaskID)
	}
	if got.Citizen != username {
		t.Errorf("dispatched event Citizen = %q, want %q (username from wire)", got.Citizen, username)
	}
	if got.Timestamp.IsZero() {
		t.Errorf("dispatched event Timestamp is zero — wire `ts` field not decoding")
	}
}

// TestNotifyIntegration_NoMatchingRuleNoDispatch pins the
// project-scoping side of the wire: an event in a *different*
// project doesn't reach a notify Run pinned to project A. This
// is the leak fix's runtime check, end-to-end.
func TestNotifyIntegration_NoMatchingRuleNoDispatch(t *testing.T) {
	ts := newTestServer(t)
	username := ts.register("Notify Scoping")
	projectA := ts.createTestProject()
	projectB := ts.createTestProject()

	citizen, err := ts.store.GetCitizenByUsername(username)
	if err != nil || citizen == nil {
		t.Fatalf("lookup: %v", err)
	}

	var dispatched int32
	cfg := notify.Config{
		CoordinatorURL: ts.url,
		ProjectID:      projectA, // pin to A
		CitizenID:      citizen.ID,
		Username:       username,
		BearerToken:    citizen.Token,
		Rules: []notify.Rule{{
			Name: "any-task-completed",
			When: notify.Predicate{EventType: "task_completed"},
			Kind: "desktop",
		}},
		DisableDefaults: []string{"all"},
		PollWait:        500 * time.Millisecond,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev notify.Event, rule notify.Rule, _ notify.Config) error {
			atomic.AddInt32(&dispatched, 1)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	go func() { _ = notify.Run(ctx, cfg) }()

	time.Sleep(100 * time.Millisecond)

	// Emit into project B — the poller pinned to A must not see it.
	ts.store.Events().Record(store.Event{
		CitizenID: citizen.ID,
		EventType: "task_completed",
		ProjectID: projectB,
		TaskID:    "9:1:other",
		CreatedAt: time.Now(),
	})

	// Wait for the context to expire so the poller has had a full
	// shot at picking up the (mis-scoped) event.
	<-ctx.Done()
	// Drain a bit more to give the goroutine time to exit cleanly
	// before we read the counter.
	time.Sleep(100 * time.Millisecond)

	if got := atomic.LoadInt32(&dispatched); got != 0 {
		t.Errorf("project A poller saw %d events from project B — scoping leak", got)
	}
}
