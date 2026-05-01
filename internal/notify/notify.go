// Package notify is the fat-client notification subsystem —
// the "Thunderbird" half of the server-stores-client-routes
// design (see docs/notifications.md).
//
// Run() long-polls the coordinator's events endpoint for one
// project, matches each event against the rules in the Config,
// and dispatches matches to local adapters (desktop notify,
// shell command, etc.). The package is self-contained: callers
// pass a Config, Run blocks until ctx is cancelled or a non-
// recoverable error fires.
//
// Two callers are anticipated:
//
//   - Tier 1 (default): the `enju mcp` fat-client invokes Run
//   as a goroutine on startup, using the running citizen's
//   bearer token. Notifications work whenever the MCP host is
//   open. Single-process, zero ceremony.
//   - Tier 2 (post-v1): a standalone `enju notify` subcommand
//   wraps Run in its own main loop, runs as a registered
//   child-bot citizen. Useful for 24/7 pings even when the
//   MCP host is closed (overnight runs, hosted-mode bridges).
//
// The package doesn't know which tier is calling — Config is
// the only input. The same code path handles both.
package notify

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// Config holds the runtime parameters for one notify loop.
// Each field documents its own semantics.
type Config struct {
	// CoordinatorURL is the base URL of the coordinator,
	// e.g. "http://localhost:8000". The events endpoint
	// path is constructed internally.
	CoordinatorURL string

	// ProjectID scopes the notify loop to one project. Run
	// long-polls /api/v1/projects/{ProjectID}/events. Callers
	// that want notifications across multiple projects spawn
	// one Run goroutine per project.
	ProjectID int64

	// CitizenID is the running citizen's identity. Used to
	// resolve "me" in default-rule predicates (Phase 4b).
	// The bearer token must belong to this citizen.
	CitizenID int64

	// Username is the running citizen's username, used to
	// match string-keyed event fields like assign_to.
	Username string

	// BearerToken authenticates the long-poll requests. Same
	// shape as any other coordinator API call.
	BearerToken string

	// Rules is the list of user-defined rules to evaluate per
	// event. The notify loop composes these with the compiled-
	// in Layer 1 defaults (see defaults.go); both layers fire
	// on matching events. Empty Rules is fine — defaults still
	// run unless DisableDefaults includes "all".
	Rules []Rule

	// DisableDefaults turns off built-in defaults by name. The
	// literal "all" disables every default. Lets users opt out
	// of noisy defaults without abandoning the rest of the
	// platform-pulse story.
	DisableDefaults []string

	// RateLimits overrides per-rule rate-limit policy by name.
	// Applies to defaults and user rules uniformly. Unset rules
	// fall back to the table in ratelimit.go.
	RateLimits map[string]rateLimit

	// StateFile is the path where Run persists last_seq across
	// restarts. Writes are atomic (tmp + rename). Empty path
	// means "don't persist" — the daemon resumes from the
	// current head on every restart.
	StateFile string

	// PollWait is the per-request long-poll duration ("?wait=").
	// Coordinator clamps to its longPollMax; values <= 0 fall
	// back to 30s.
	//
	// Tuning note: PollWait must stay below the coordinator's
	// HTTP middleware request timeout (performance.http_request_
	// timeout in enju.conf, default 30s). When PollWait exceeds
	// that, the middleware kills the connection mid-wait and the
	// client sees a transport error — the long-poll never gets
	// to return its response. The coordinator self-clamps via
	// longPollMax = httpTimeout - 5s margin, so callers using
	// the default get the safe behavior; callers raising PollWait
	// past 25s should also raise the server's HTTP timeout.
	PollWait time.Duration

	// HTTPClient lets callers inject a client (e.g. for
	// testing). nil → http.DefaultClient.
	HTTPClient *http.Client

	// Logger receives diagnostic output. nil → slog.Default().
	Logger *slog.Logger

	// Dispatcher overrides the default adapter dispatch (which
	// shells out to notify-send / osascript / etc). Tests inject
	// a capturing fn so they don't need a desktop environment.
	// nil → use the built-in dispatcher.
	Dispatcher func(Event, Rule, Config) error
}

// Run is the long-poll loop. Blocks until ctx is cancelled or
// an unrecoverable error fires. Transient errors (network
// blips, coordinator restarts) are logged and retried with
// backoff; the function only returns on ctx.Err() or
// configuration errors that can't be retried out of.
//
// The contract:
//
//  1. On startup, load state.last_seq (or default to "now" via
//   a fresh head fetch — TODO: implement skip-to-head in 4b).
//  2. For each tick: long-poll /events?wait=PollWait&since=...
//  3. For each returned event: match against Rules and dispatch.
//  4. Persist last_seq after each batch (atomic write).
//  5. Repeat forever.
//
// Failures dispatch-side don't stop the loop: a broken adapter
// logs and continues. Only ctx cancellation or a config error
// terminates Run.
func Run(ctx context.Context, cfg Config) error {
	if cfg.CoordinatorURL == "" {
		return fmt.Errorf("notify.Run: CoordinatorURL is required")
	}
	if cfg.ProjectID <= 0 {
		return fmt.Errorf("notify.Run: ProjectID is required")
	}
	if cfg.BearerToken == "" {
		return fmt.Errorf("notify.Run: BearerToken is required")
	}
	httpClient := cfg.HTTPClient
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	pollWait := cfg.PollWait
	if pollWait <= 0 {
		pollWait = 30 * time.Second
	}

	state, _ := loadState(cfg.StateFile) // missing file = empty state, fine

	// Compose user rules with Layer 1 defaults. Defaults come
	// first so they're evaluated first; ordering doesn't affect
	// outcomes (all matching rules dispatch independently).
	defaults := effectiveDefaults(cfg.DisableDefaults)
	allRules := make([]Rule, 0, len(defaults)+len(cfg.Rules))
	allRules = append(allRules, defaults...)
	allRules = append(allRules, cfg.Rules...)

	limiter := newRateLimiter(cfg.RateLimits)

	logger.Info("notify loop started",
		"project_id", cfg.ProjectID,
		"citizen", cfg.Username,
		"since", state.LastSeen,
		"defaults", len(defaults),
		"user_rules", len(cfg.Rules),
	)

	const errorBackoff = 5 * time.Second
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		events, err := pollEvents(ctx, httpClient, cfg, pollWait, state.LastSeen)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			logger.Warn("notify poll failed; backing off", "error", err)
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(errorBackoff):
			}
			continue
		}

		dispatcher := cfg.Dispatcher
		if dispatcher == nil {
			dispatcher = dispatch
		}
		for _, ev := range events {
			matched := matchRulesAgainst(allRules, ev, cfg)
			for _, rule := range matched {
				if !limiter.allow(rule, cfg) {
					logger.Debug("notify rate limit hit",
						"rule", rule.Name, "event_type", ev.Type)
					continue
				}
				if err := dispatcher(ev, rule, cfg); err != nil {
					logger.Warn("notify dispatch failed",
						"rule", rule.Name, "event_type", ev.Type, "error", err)
				}
			}
			// Cursor advance: bump 1ns past the event timestamp.
			// The coordinator's /events?since= filter is inclusive
			// (`created_at >= since`), so storing the raw timestamp
			// would re-fetch the same event on the next poll and
			// dispatch it forever. +1ns shifts the cursor strictly
			// past this event without skipping any (timestamps are
			// nanosecond-resolution wall-clock, near-zero collision
			// risk in real workloads).
			if next := ev.Timestamp.Add(time.Nanosecond); next.After(state.LastSeen) {
				state.LastSeen = next
			}
		}

		// Persist after each batch — bounded staleness on crash.
		// Atomic write so a half-finished file isn't readable.
		if err := saveState(cfg.StateFile, state); err != nil {
			logger.Warn("notify state save failed", "error", err)
		}
	}
}

// pollEvents issues one long-poll request and returns the
// decoded events. since is the last-seen timestamp; the
// coordinator returns events strictly after it.
func pollEvents(ctx context.Context, client *http.Client, cfg Config, wait time.Duration, since time.Time) ([]Event, error) {
	q := url.Values{}
	q.Set("wait", wait.String())
	if !since.IsZero() {
		// Fixed-width nanos. time.RFC3339Nano (".999999999") strips
		// trailing zeros, so a cursor of e.g. .387655150 serializes
		// as .38765515 — which the server then re-parses as the
		// smaller fractional 387,651,500ns and re-matches the same
		// event. The .000000000 layout pins all 9 digits and avoids
		// the round-trip precision loss.
		q.Set("since", since.UTC().Format("2006-01-02T15:04:05.000000000Z07:00"))
	}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/events?%s",
		strings.TrimRight(cfg.CoordinatorURL, "/"), cfg.ProjectID, q.Encode())

	req, err := http.NewRequestWithContext(ctx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.BearerToken)

	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("HTTP %d: %s", resp.StatusCode, string(body))
	}
	var events []Event
	if err := json.Unmarshal(body, &events); err != nil {
		return nil, fmt.Errorf("decoding events: %w", err)
	}
	return events, nil
}

// Event mirrors the wire shape of /events response items. We
// keep this local rather than importing internal/store to keep
// the notify package buildable without the whole store package
// in dependency closure (matters for the future Tier 2
// standalone binary).
type Event struct {
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	Citizen   string    `json:"citizen,omitempty"`
	Metadata  any       `json:"metadata,omitempty"`
}
