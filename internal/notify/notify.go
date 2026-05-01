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
	"strconv"
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

	// ProjectDir is the project's local clone directory (e.g.
	// ~/.enju/workspaces/tp53-paper-5/). All project-scoped state
	// — the cursor file, the live.jsonl event log, the user
	// rules file — lives under {ProjectDir}/enju/. Empty means
	// "don't persist anything to disk"; tests that supply
	// in-memory configs leave it blank.
	//
	// Invariant: when ProjectDir is set, StateFile is ignored —
	// the cursor lives at {ProjectDir}/enju/events/cursor.json.
	// StateFile remains for explicit-path tests.
	ProjectDir string

	// StateFile is the legacy explicit-path cursor used by tests
	// that set up cursor state without a full project workspace.
	// In production callers, leave empty; ProjectDir derives the
	// path automatically.
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

	state, _ := loadState(cursorPath(cfg)) // missing file = empty state, fine

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
		"since_seq", state.LastSeq,
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

		events, err := pollEvents(ctx, httpClient, cfg, pollWait, state.LastSeq)
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

		// Server returns newest-first; iterate oldest-first so
		// cursor advances monotonically and dispatch order matches
		// causal order.
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}

		dispatcher := cfg.Dispatcher
		if dispatcher == nil {
			dispatcher = dispatch
		}
		jsonlPath := liveJSONLPath(cfg)
		for _, ev := range events {
			// Append to local audit log BEFORE dispatch. If we
			// crash between log-write and dispatch, the next run
			// catches up via cursor — events aren't lost. If we
			// crash between dispatch and log-write, the user got
			// notified but the local log lacks the entry — also
			// recoverable on next poll because the cursor hasn't
			// advanced yet.
			if jsonlPath != "" {
				if err := appendEventToLog(jsonlPath, ev); err != nil {
					logger.Warn("notify log append failed", "path", jsonlPath, "error", err)
				}
			}
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
			// Cursor advance: server's since_seq filter is strict
			// `>`, so saving the event's seq is exact. Next poll
			// returns "everything strictly after this." No +1
			// dance, no edge cases.
			if ev.Seq > state.LastSeq {
				state.LastSeq = ev.Seq
			}
		}

		// Persist after each batch — bounded staleness on crash.
		// Atomic write so a half-finished file isn't readable.
		if err := saveState(cursorPath(cfg), state); err != nil {
			logger.Warn("notify state save failed", "error", err)
		}
	}
}

// pollEvents issues one long-poll request and returns the
// decoded events. sinceSeq is the strict-`>` cursor; the
// coordinator returns events with seq > sinceSeq.
func pollEvents(ctx context.Context, client *http.Client, cfg Config, wait time.Duration, sinceSeq int64) ([]Event, error) {
	q := url.Values{}
	q.Set("wait", wait.String())
	if sinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(sinceSeq, 10))
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
	Seq       int64     `json:"seq"`
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	Citizen   string    `json:"citizen,omitempty"`
	Metadata  any       `json:"metadata,omitempty"`
}


// cursorPath resolves the cursor file location for this loop.
// Project-scoped path takes precedence over the legacy explicit
// StateFile (which test fixtures still rely on).
func cursorPath(cfg Config) string {
	if cfg.StateFile != "" {
		return cfg.StateFile
	}
	if cfg.ProjectDir == "" {
		return ""
	}
	return cfg.ProjectDir + "/enju/events/cursor.json"
}

// UserConfigPath returns the canonical project-scoped notify.yaml
// location for a given project clone dir. Exported so callers
// outside the package (notifySession) can compute it from a path
// they have but resolve the layout convention from one place.
func UserConfigPath(projectDir string) string {
	if projectDir == "" {
		return ""
	}
	return projectDir + "/enju/notify.yaml"
}

// liveJSONLPath resolves the append-only event log path. Empty
// when ProjectDir is unset (writes skip cleanly).
func liveJSONLPath(cfg Config) string {
	if cfg.ProjectDir == "" {
		return ""
	}
	return cfg.ProjectDir + "/enju/events/live.jsonl"
}
