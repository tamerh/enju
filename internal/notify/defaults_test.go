package notify

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// TestEffectiveDefaultsAllAndIndividual pins the opt-out
// surface: the "all" sentinel kills every default; individual
// names disable just that one.
func TestEffectiveDefaultsAllAndIndividual(t *testing.T) {
	allDefaults := compiledDefaults()
	if len(allDefaults) == 0 {
		t.Skip("no Layer 1 defaults compiled in yet — test will activate as defaults grow")
	}

	// No disable → all defaults present.
	if got := effectiveDefaults(nil); len(got) != len(allDefaults) {
		t.Errorf("nil disable list: got %d defaults, want %d", len(got), len(allDefaults))
	}

	// "all" → none.
	if got := effectiveDefaults([]string{"all"}); len(got) != 0 {
		t.Errorf("'all' disable: got %d defaults, want 0", len(got))
	}

	// Disable one specific → all but that one.
	one := allDefaults[0].Name
	got := effectiveDefaults([]string{one})
	if len(got) != len(allDefaults)-1 {
		t.Errorf("disabling %q: got %d defaults, want %d", one, len(got), len(allDefaults)-1)
	}
	for _, r := range got {
		if r.Name == one {
			t.Errorf("default %q should have been filtered, still present", one)
		}
	}
}

// TestRunFiresLayer1Default pins that compiled-in defaults
// fire end-to-end without any user-defined rules. This is the
// "platform pulse" promise from the design doc — out of the
// box, the daemon notifies on relevant events.
func TestRunFiresLayer1Default(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Event matching the my_task_completed default.
		_ = json.NewEncoder(w).Encode([]Event{{
			Timestamp: time.Now().UTC(),
			Type:      "task_completed",
			Subtype:   "answer",
			TaskID:    "1:1:draft",
			Citizen:   "tamer",
		}})
	}))
	defer srv.Close()

	var mu sync.Mutex
	var rulesFired []string
	cfg := Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		Username:       "tamer",
		BearerToken:    "test-token",
		// No user rules — pure Layer 1 default exercise.
		Rules:      nil,
		PollWait:   50 * time.Millisecond,
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev Event, rule Rule, cfg Config) error {
			mu.Lock()
			defer mu.Unlock()
			rulesFired = append(rulesFired, rule.Name)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	_ = Run(ctx, cfg)

	mu.Lock()
	defer mu.Unlock()
	if len(rulesFired) == 0 {
		t.Fatal("expected at least one Layer 1 default to fire, got none")
	}
	if rulesFired[0] != "my_task_completed" {
		t.Errorf("expected my_task_completed default, got %q", rulesFired[0])
	}
}

// TestRunDisableDefaultsAll pins the kill-switch: "all" in
// DisableDefaults means no default ever fires, even on a
// matching event.
func TestRunDisableDefaultsAll(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]Event{{
			Timestamp: time.Now().UTC(),
			Type:      "task_completed",
			Citizen:   "tamer",
		}})
	}))
	defer srv.Close()

	var dispatched int32
	cfg := Config{
		CoordinatorURL:  srv.URL,
		ProjectID:       1,
		Username:        "tamer",
		BearerToken:     "test-token",
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
		t.Errorf("DisableDefaults=[all] should suppress everything, got %d dispatches", got)
	}
}

// TestRateLimiterDropsExcess pins the sliding window: more
// than Max dispatches in Window get rejected. Uses a generous
// limit to keep timing predictable; tighter limits are
// covered indirectly by the run-loop tests.
func TestRateLimiterDropsExcess(t *testing.T) {
	rl := newRateLimiter(map[string]rateLimit{
		"test-rule": {Window: 100 * time.Millisecond, Max: 3},
	})
	rule := Rule{Name: "test-rule"}
	cfg := Config{CitizenID: 1, ProjectID: 1}

	// First 3 calls allowed.
	for i := 0; i < 3; i++ {
		if !rl.allow(rule, cfg) {
			t.Errorf("call %d should be allowed (within Max=3)", i+1)
		}
	}
	// 4th call hits the limit.
	if rl.allow(rule, cfg) {
		t.Error("4th call within window should be rate-limited")
	}

	// After the window expires, allowance resets.
	time.Sleep(110 * time.Millisecond)
	if !rl.allow(rule, cfg) {
		t.Error("call after window expiry should be allowed")
	}
}

// TestRateLimiterScopesToCitizenAndProject pins that two
// different citizens don't share each other's budget. Same
// for projects: a noisy project-1 doesn't starve project-2.
func TestRateLimiterScopesToCitizenAndProject(t *testing.T) {
	rl := newRateLimiter(map[string]rateLimit{
		"test-rule": {Window: time.Second, Max: 2},
	})
	rule := Rule{Name: "test-rule"}
	c1 := Config{CitizenID: 1, ProjectID: 1}
	c2 := Config{CitizenID: 2, ProjectID: 1} // different citizen
	c3 := Config{CitizenID: 1, ProjectID: 2} // same citizen, different project

	// Saturate citizen 1 / project 1.
	rl.allow(rule, c1)
	rl.allow(rule, c1)
	if rl.allow(rule, c1) {
		t.Error("c1 should be rate-limited after 2 calls")
	}
	// Citizen 2 still has its full budget.
	if !rl.allow(rule, c2) {
		t.Error("c2 should not be affected by c1 saturation")
	}
	// Same citizen, different project: also independent.
	if !rl.allow(rule, c3) {
		t.Error("c3 should not be affected by c1/project1 saturation")
	}
}

// TestRateLimiterFallback pins the lookup precedence:
// custom > defaults table > general fallback. Custom override
// always wins.
func TestRateLimiterFallback(t *testing.T) {
	custom := map[string]rateLimit{
		"my_task_completed": {Window: time.Second, Max: 1},
	}
	rl := newRateLimiter(custom)
	rule := Rule{Name: "my_task_completed"}
	cfg := Config{CitizenID: 1, ProjectID: 1}

	if !rl.allow(rule, cfg) {
		t.Error("first call should pass")
	}
	if rl.allow(rule, cfg) {
		t.Error("second call should be limited (custom Max=1 overrides defaults table Max=10)")
	}
}

// TestRunRateLimitSuppressesBurst is the integration check:
// even when many matching events arrive in a short burst, the
// rate limiter caps dispatches at the rule's Max. This is the
// "stress test that fires 50 issues" → not 50 popups guarantee.
func TestRunRateLimitSuppressesBurst(t *testing.T) {
	// Coordinator returns 20 task_completed events on first
	// poll, then idles.
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := pollCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			now := time.Now().UTC()
			events := make([]Event, 20)
			for i := range events {
				events[i] = Event{
					Timestamp: now.Add(time.Duration(i) * time.Millisecond),
					Type:      "task_completed",
					TaskID:    "1:1:draft",
					Citizen:   "tamer",
				}
			}
			_ = json.NewEncoder(w).Encode(events)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	var dispatched int32
	cfg := Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		CitizenID:      42,
		Username:       "tamer",
		BearerToken:    "test-token",
		// Tighten my_task_completed to 5 in 10s for the test.
		// Default is 10/min which would still cap us; explicit
		// override pins behavior to deterministic numbers.
		RateLimits: map[string]rateLimit{
			"my_task_completed": {Window: 10 * time.Second, Max: 5},
		},
		StateFile:  filepath.Join(t.TempDir(), "state.json"),
		PollWait:   100 * time.Millisecond,
		HTTPClient: srv.Client(),
		Logger:     slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev Event, rule Rule, cfg Config) error {
			atomic.AddInt32(&dispatched, 1)
			return nil
		},
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	_ = Run(ctx, cfg)

	got := atomic.LoadInt32(&dispatched)
	if got != 5 {
		t.Errorf("expected exactly 5 dispatches (Max=5 over 20 burst events), got %d", got)
	}
}

// TestProjectPulseDefaultsFire pins the new Layer 1 additions.
// Each event type should match its named default and fire to the
// dispatcher. Pre-Phase 4f the user only got pinged on their own
// task resolutions; this test locks in the broader "platform
// pulse" set.
func TestProjectPulseDefaultsFire(t *testing.T) {
	cases := []struct {
		eventType    string
		wantRuleName string
	}{
		{"branch_merged", "branch_merged"},
		{"issue_filed", "issue_filed"},
		{"cycle_budget_exhausted", "cycle_budget_exhausted"},
		{"task_request_changes", "task_request_changes"},
		{"run_completed", "run_completed"},
		{"run_paused", "run_paused"},
		{"run_resumed", "run_resumed"},
	}
	for _, tc := range cases {
		t.Run(tc.eventType, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode([]Event{{
					Timestamp: time.Now().UTC(),
					Type:      tc.eventType,
					TaskID:    "1:1:t",
					Citizen:   "alice",
				}})
			}))
			defer srv.Close()

			var fired []string
			var mu sync.Mutex
			cfg := Config{
				CoordinatorURL: srv.URL,
				ProjectID:      1,
				Username:       "tamer",
				BearerToken:    "test-token",
				PollWait:       50 * time.Millisecond,
				HTTPClient:     srv.Client(),
				Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
				Dispatcher: func(ev Event, rule Rule, cfg Config) error {
					mu.Lock()
					defer mu.Unlock()
					fired = append(fired, rule.Name)
					return nil
				},
			}
			ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
			defer cancel()
			_ = Run(ctx, cfg)

			mu.Lock()
			defer mu.Unlock()
			found := false
			for _, name := range fired {
				if name == tc.wantRuleName {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("event %q: expected default %q to fire, got %v", tc.eventType, tc.wantRuleName, fired)
			}
		})
	}
}

// TestCycleBudgetExhaustedNoRateLimit pins that the "critical
// signal" carve-out works: Max=0 means no limit, so 50 emissions
// of cycle_budget_exhausted all dispatch (vs hitting the 5/min
// or 10/min cap that would suppress the most important signal).
func TestCycleBudgetExhaustedNoRateLimit(t *testing.T) {
	var pollCount atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		count := pollCount.Add(1)
		w.Header().Set("Content-Type", "application/json")
		if count == 1 {
			now := time.Now().UTC()
			events := make([]Event, 50)
			for i := range events {
				events[i] = Event{
					Timestamp: now.Add(time.Duration(i) * time.Millisecond),
					Type:      "cycle_budget_exhausted",
					TaskID:    "1:1:t",
				}
			}
			_ = json.NewEncoder(w).Encode(events)
			return
		}
		_, _ = w.Write([]byte("[]"))
	}))
	defer srv.Close()

	var dispatched int32
	cfg := Config{
		CoordinatorURL: srv.URL,
		ProjectID:      1,
		Username:       "tamer",
		BearerToken:    "test-token",
		StateFile:      filepath.Join(t.TempDir(), "state.json"),
		PollWait:       100 * time.Millisecond,
		HTTPClient:     srv.Client(),
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
		Dispatcher: func(ev Event, rule Rule, cfg Config) error {
			atomic.AddInt32(&dispatched, 1)
			return nil
		},
	}
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_ = Run(ctx, cfg)

	if got := atomic.LoadInt32(&dispatched); got != 50 {
		t.Errorf("cycle_budget_exhausted has Max=0 (no limit); 50 emissions should all dispatch, got %d", got)
	}
}
