// Package notify is the fat-client poll-and-record substrate
// that maintains a per-project event log on local disk.
//
// Run() long-polls the coordinator's events endpoint for one
// project and appends each received event as a JSON line to
// {ProjectDir}/.enju/events/live.jsonl, advancing the cursor in
// {ProjectDir}/.enju/events/cursor.json after every batch.
//
// That's the entire job. The historical "notifications" tool
// (with hardcoded interesting-events rules + read/unread
// tracking) was removed; consumers now read live.jsonl
// directly: enju_inbox projects "what should I act on" out of
// it, and enju_recent_events queries the coordinator (with
// for_me=true for citizen-scoped views).
//
// The package is self-contained: callers pass a Config, Run
// blocks until ctx is cancelled or a non-recoverable error
// fires. No adapters, no dispatch, no in-process consumers —
// downstream consumers (the inbox tool, future plugin
// processes) read live.jsonl independently.
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

	// CitizenID is the running citizen's identity. The bearer
	// token must belong to this citizen.
	CitizenID int64

	// Username is the running citizen's username, used to
	// match string-keyed event fields like assign_to.
	Username string

	// BearerToken is the static fallback token. Used when
	// BearerTokenFn is nil (tests, simple callers). Production
	// callers (mcpserver's notifySession) should set
	// BearerTokenFn instead so token rotations from the surrounding
	// apiClient's auto-reregister flow propagate to the poll loop.
	BearerToken string

	// BearerTokenFn returns the current live bearer token on each
	// call. When set, takes precedence over BearerToken — every
	// HTTP request fetches a fresh value, so a token rotation
	// (apiClient's auto-reregister updating its atomic.Value)
	// reaches the next poll without needing to restart MCP. The
	// real fix to the "stale token after coordinator DB wipe"
	// failure mode the test team hit twice. Nil → fall back to
	// BearerToken (the static string).
	BearerTokenFn func() string

	// ProjectDir is the project's local clone directory (e.g.
	// ~/.enju/workspaces/tp53-paper-5/). All project-scoped state
	// — the cursor file, the live.jsonl event log, the user
	// rules file — lives under {ProjectDir}/enju/. Empty means
	// "don't persist anything to disk"; tests that supply
	// in-memory configs leave it blank.
	//
	// Invariant: when ProjectDir is set, StateFile is ignored —
	// the cursor lives at {ProjectDir}/.enju/events/cursor.json.
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
}

// Run is the poll loop. Blocks until ctx is cancelled. Transient
// errors (network blips, coordinator restarts, stale tokens) are
// logged and retried with backoff. Returns only on ctx.Err() or
// missing-required-config.
//
// What it does, end to end:
//
//  1. Load saved cursor (last seq seen) from
//     {ProjectDir}/.enju/events/cursor.json.
//  2. Long-poll /events?since_seq=N&wait=PollWait.
//  3. Append each returned event as a JSON line to
//     {ProjectDir}/.enju/events/live.jsonl.
//  4. Advance cursor to the highest seen seq, persist atomically.
//  5. Repeat.
//
// Display happens elsewhere — enju_inbox projects live.jsonl
// into the citizen's actionable queue, enju_recent_events
// goes through the coordinator. Run is just the
// poll-and-record substrate.
func Run(ctx context.Context, cfg Config) error {
	if cfg.CoordinatorURL == "" {
		return fmt.Errorf("notify.Run: CoordinatorURL is required")
	}
	if cfg.ProjectID <= 0 {
		return fmt.Errorf("notify.Run: ProjectID is required")
	}
	if cfg.BearerToken == "" && cfg.BearerTokenFn == nil {
		return fmt.Errorf("notify.Run: BearerToken or BearerTokenFn is required")
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

	logger.Info("notify loop started",
		"project_id", cfg.ProjectID,
		"citizen", cfg.Username,
		"since_seq", state.LastSeq,
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
			// 401/403 means the coordinator currently rejects our
			// bearer (DB wipe + re-register sequence). With
			// BearerTokenFn wired, the next poll picks up the
			// rotated token and recovers automatically.
			if isAuthError(err) && cfg.BearerTokenFn != nil {
				logger.Warn("notify: poll auth-rejected; will retry with live token on next cycle",
					"project_id", cfg.ProjectID, "error", err)
			} else {
				logger.Warn("notify poll failed; backing off", "error", err)
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(errorBackoff):
			}
			continue
		}

		// Server returns newest-first; iterate oldest-first so
		// cursor advances monotonically.
		for i, j := 0, len(events)-1; i < j; i, j = i+1, j-1 {
			events[i], events[j] = events[j], events[i]
		}

		jsonlPath := liveJSONLPath(cfg)
		for _, ev := range events {
			if jsonlPath != "" {
				if err := appendEventToLog(jsonlPath, ev); err != nil {
					logger.Warn("notify log append failed", "path", jsonlPath, "error", err)
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
//
// Per-request deadline: each poll caps at wait + pollSlack so a
// silently-broken TCP connection (NAT timeout, proxy hold, server
// hang) can never wedge the loop indefinitely. Without this, a
// dropped long-poll connection leaves the client awaiting a
// response that will never come — the outer loop never retries
// and live.jsonl falls behind events.db. Tester report: cursor
// stuck at seq=13 with seqs 14-17 visible on the coordinator
// but never reaching the file. Server side already self-clamps
// to its own httpTimeout-5s; the client must also self-clamp so
// transport-level hangs don't bypass the server's limit.
func pollEvents(ctx context.Context, client *http.Client, cfg Config, wait time.Duration, sinceSeq int64) ([]Event, error) {
	q := url.Values{}
	q.Set("wait", wait.String())
	if sinceSeq > 0 {
		q.Set("since_seq", strconv.FormatInt(sinceSeq, 10))
	}
	endpoint := fmt.Sprintf("%s/api/v1/projects/%d/events?%s",
		strings.TrimRight(cfg.CoordinatorURL, "/"), cfg.ProjectID, q.Encode())

	const pollSlack = 10 * time.Second
	reqCtx, cancel := context.WithTimeout(ctx, wait+pollSlack)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, "GET", endpoint, nil)
	if err != nil {
		return nil, err
	}
	tok := cfg.BearerToken
	if cfg.BearerTokenFn != nil {
		// Live read: picks up token rotations performed by the
		// surrounding apiClient's auto-reregister flow without
		// restarting the poll loop.
		if live := cfg.BearerTokenFn(); live != "" {
			tok = live
		}
	}
	req.Header.Set("Authorization", "Bearer "+tok)

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
	// AssignTo is the username the task is assigned to. The
	// coordinator hoists this from event metadata to a top-level
	// wire field so the predicate matcher can treat it like the
	// other identity fields. Empty when the event isn't task-
	// scoped or the task has no assignee.
	AssignTo string `json:"assign_to,omitempty"`
	Metadata any    `json:"metadata,omitempty"`
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
	return cfg.ProjectDir + "/.enju/events/cursor.json"
}

// liveJSONLPath resolves the append-only event log path. Empty
// when ProjectDir is unset (writes skip cleanly).
func liveJSONLPath(cfg Config) string {
	if cfg.ProjectDir == "" {
		return ""
	}
	return cfg.ProjectDir + "/.enju/events/live.jsonl"
}

// isAuthError reports whether a poll error came from the
// coordinator rejecting the bearer token. The notify loop
// returns on this rather than backing off forever — auth issues
// don't self-heal because notify caches the token at boot.
func isAuthError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "HTTP 401") || strings.Contains(msg, "HTTP 403")
}

