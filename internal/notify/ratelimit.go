package notify

// Sliding-window rate limiter. Keys notifications by
// (rule, citizen, project) tuple to prevent storms — without
// this, a stress test that fires 50 task_completed events in
// 10 seconds would produce 50 desktop popups, killing the
// "alive by default" property.
//
// v1: drop excess silently when over the limit. The design
// doc describes a "digest at window close" extension (e.g.
// "8 more issues filed") for noisy defaults; that's a Phase
// 4c addition once we see real noise patterns.

import (
	"fmt"
	"sync"
	"time"
)

// rateLimit is the per-rule policy: at most Max dispatches
// within any rolling Window. Defaults defined by name in
// defaultRateLimits below; user rules use a generic fallback.
type rateLimit struct {
	Window time.Duration
	Max    int
}

// defaultRateLimits maps rule names to their per-rule policy.
// Tunable via Config.RateLimits; defaults shown here are the
// design doc's starting point.
//
// Tighter limits for "individually meaningful" events
// (assigned tasks); looser for high-rate categories (issue
// filings) with the assumption that bursts come in cohorts.
var defaultRateLimits = map[string]rateLimit{
	"my_task_completed": {Window: time.Minute, Max: 10},
	"my_task_failed":    {Window: time.Minute, Max: 5},

	// project-pulse defaults — looser than "my-X" since they fire
	// on any actor's action, but bounded so a runaway emitter
	// doesn't drown the desktop.
	"branch_merged":          {Window: time.Minute, Max: 10},
	"issue_filed":            {Window: time.Minute, Max: 5},
	"task_request_changes":   {Window: time.Minute, Max: 5},
	"run_completed":          {Window: time.Minute, Max: 5},
	"run_paused":             {Window: time.Minute, Max: 5},
	"run_resumed":            {Window: time.Minute, Max: 5},

	// cycle_budget_exhausted: no limit. Fires once per run by
	// definition (the run auto-pauses), so dropping it would mask
	// the most critical signal in the system. Max=0 == disabled.
	"cycle_budget_exhausted": {Window: time.Minute, Max: 0},
}

// generalFallbackLimit applies to any rule (built-in or user)
// without a specific entry in defaultRateLimits or
// Config.RateLimits. 10/min is generous enough to avoid
// surprising user rules but stringent enough to bound any
// runaway emitter.
var generalFallbackLimit = rateLimit{Window: time.Minute, Max: 10}

// rateLimiter is the goroutine-safe sliding-window tracker.
// One instance per Run loop; dispatch consults it before
// firing the adapter.
type rateLimiter struct {
	mu      sync.Mutex
	windows map[string][]time.Time
	custom  map[string]rateLimit // overrides, populated from Config.RateLimits
}

func newRateLimiter(custom map[string]rateLimit) *rateLimiter {
	return &rateLimiter{
		windows: make(map[string][]time.Time),
		custom:  custom,
	}
}

// allow records a dispatch attempt and returns true if it's
// within budget. Returns false if the (rule, citizen, project)
// window is full — caller drops the dispatch silently.
//
// Implementation: keep a slice of recent timestamps per key,
// trim expired entries on each call, compare count against the
// rule's Max. O(window-events) per call; for v1 traffic
// patterns (tens of events/sec across all rules) the constant
// factor is negligible.
func (rl *rateLimiter) allow(rule Rule, cfg Config) bool {
	limit := rl.lookupLimit(rule.Name)
	if limit.Max <= 0 || limit.Window <= 0 {
		return true // disabled limit → always allow
	}
	key := fmt.Sprintf("%s:%d:%d", rule.Name, cfg.CitizenID, cfg.ProjectID)

	rl.mu.Lock()
	defer rl.mu.Unlock()

	now := time.Now()
	cutoff := now.Add(-limit.Window)

	// Drop expired timestamps in place.
	fresh := rl.windows[key][:0]
	for _, t := range rl.windows[key] {
		if t.After(cutoff) {
			fresh = append(fresh, t)
		}
	}

	if len(fresh) >= limit.Max {
		rl.windows[key] = fresh
		return false
	}
	fresh = append(fresh, now)
	rl.windows[key] = fresh
	return true
}

// lookupLimit resolves a rule name to its rate-limit policy.
// Order: Config-supplied custom > defaults table > general
// fallback. Custom takes precedence so users can tighten or
// loosen any rule (default or their own) without touching code.
func (rl *rateLimiter) lookupLimit(name string) rateLimit {
	if l, ok := rl.custom[name]; ok {
		return l
	}
	if l, ok := defaultRateLimits[name]; ok {
		return l
	}
	return generalFallbackLimit
}
