package bots

// AutoRunManager bundles the auto_bots preflight + rollback +
// post-POST hookup that enju_create_run runs around the coord
// /runs POST. Used by both the MCP handler (handleCreateRun)
// and the CLI (cmd/enju/go.go:createRun) so the two paths can't
// drift.
//
// Lifetime: one manager instance per create_run call. Cheap to
// construct; the supervisor it wraps is process-scoped.
//
// REV.1 invariant — by construction, not by comment.
// Pre-extract, Rollback took an explicit slice argument and a
// known-bad refactor passed the wider autoBotNames list there,
// killing operator-owned bots that came back from Supervisor.Start
// as AlreadyRunning. The fix in this manager: Preflight stores
// the strict freshStarts subset on the manager, and Rollback
// reads from there. Callers don't see the slice — they can't
// pass the wrong one.
//
// The wider autoBotNames list (every bot that became ready,
// including AlreadyRunning) is exposed only via accessor
// methods that consume it intentionally (HookRunSeq, AutoBotNames
// for the result-text summary).

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"
)

// AutoRunReadyTimeout is the per-bot Preflight wait used by
// both create_run call sites (MCP handler + CLI `enju go
// --auto-bots`). Defaults to 30s; tunable in production via
// ENJU_AUTO_BOTS_TIMEOUT for first-touch demos with cold
// claude-CLI subprocesses that need longer warmup.
//
// Test path: tests construct AutoRunManager directly via
// NewAutoRunManager(... readyTimeout), bypassing this function
// — that's intentional so test cases can pin a deterministic
// timeout without depending on global env. The env-override
// path is for operator use; tests don't go through it.
func AutoRunReadyTimeout() time.Duration {
	if v := os.Getenv("ENJU_AUTO_BOTS_TIMEOUT"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return 30 * time.Second
}

// AutoRunManager owns the auto_bots state machine for one
// create_run invocation. Construct once per call; supervisor
// is shared across calls and outlives the manager.
type AutoRunManager struct {
	sup          *Supervisor
	absWorkflow  string
	coordURL     string
	projectID    int64
	readyTimeout time.Duration

	// freshStarts is the strict subset of bots THIS call
	// spawned (StartedFresh outcome from Supervisor.Start).
	// Only these are safe to Stop on rollback — bots that
	// came back AlreadyRunning are operator-owned and stay
	// alive regardless of run outcome. Set only by Preflight;
	// consumed only by Rollback.
	freshStarts []string

	// autoBotNames is every bot that completed preflight
	// (StartedFresh OR AlreadyRunning, both reached PhaseReady).
	// Used for MarkAutoRun + WatchProjectEvents so terminal
	// events decrement them. Exposed via AutoBotNames() for
	// the result-text summary.
	autoBotNames []string
}

// NewAutoRunManager constructs a manager scoped to one
// create_run invocation. absWorkflow is the absolute path to the
// workflow YAML the bot daemons read (their --workflow flag).
// coordURL is the URL the daemons authenticate against.
// readyTimeout caps WaitForReady per bot.
func NewAutoRunManager(sup *Supervisor, absWorkflow, coordURL string, projectID int64, readyTimeout time.Duration) *AutoRunManager {
	return &AutoRunManager{
		sup:          sup,
		absWorkflow:  absWorkflow,
		coordURL:     coordURL,
		projectID:    projectID,
		readyTimeout: readyTimeout,
	}
}

// Preflight starts every bot in manifest and blocks until each
// reports PhaseReady (or readyTimeout expires). Idempotent on
// re-entry — Supervisor.Start returns AlreadyRunning for bots a
// prior call already started, and those bots ride along in
// autoBotNames without joining freshStarts.
//
// On error: caller must invoke Rollback(ctx) to stop any
// freshly-started bots before returning to the user. Already-
// running bots are left alone by Rollback's design.
//
// Manifest must be non-nil and have at least one bot — the
// caller's "workflow declares no bots:" check fires before this
// call.
func (m *AutoRunManager) Preflight(ctx context.Context, manifest *Manifest) error {
	if manifest == nil || len(manifest.Bots) == 0 {
		return fmt.Errorf("preflight called with empty manifest (caller should reject earlier)")
	}
	for _, b := range manifest.Bots {
		var allow []string
		if b.MCPTools != nil {
			allow = b.MCPTools.Allow
		}
		_, outcome, serr := m.sup.Start(ctx, StartParams{
			BotName:      b.Name,
			WorkflowPath: m.absWorkflow,
			Coordinator:  m.coordURL,
			ProjectID:    m.projectID,
			AllowTools:   allow,
			StartedBy:    "auto_run",
		})
		if serr != nil {
			return fmt.Errorf("starting bot %q: %w", b.Name, serr)
		}
		if outcome == StartedFresh {
			m.freshStarts = append(m.freshStarts, b.Name)
		}
		if rerr := m.sup.WaitForReady(ctx, b.Name, m.readyTimeout); rerr != nil {
			return fmt.Errorf("bot %q: %w (check %s for daemon output)", b.Name, rerr, m.sup.LogPathFor(b.Name))
		}
		m.autoBotNames = append(m.autoBotNames, b.Name)
	}
	return nil
}

// Rollback stops the bots THIS call freshly spawned. Safe to
// call on partial-Preflight failure (some bots started, others
// failed) or on post-POST failure (everything started, coord
// returned an error). Idempotent: a second call after the first
// drained m.freshStarts is a no-op.
//
// Bots that came back as AlreadyRunning are operator-owned —
// they don't appear in m.freshStarts and stay alive.
func (m *AutoRunManager) Rollback(ctx context.Context) {
	for _, name := range m.freshStarts {
		if _, err := m.sup.Stop(ctx, name); err != nil {
			m.sup.logger().Warn("auto_run rollback: stop failed", "bot", name, "error", err)
		}
	}
	m.freshStarts = nil
}

// AutoBotNames returns the bots that completed preflight. The
// caller uses this for the result-text summary ("auto_bots: N of M
// bot(s) lost their auto-stop hook") and for any downstream
// processing that wants to know which bots were involved.
//
// Returns a copy so the caller can iterate freely without racing
// against any future internal state.
func (m *AutoRunManager) AutoBotNames() []string {
	out := make([]string, len(m.autoBotNames))
	copy(out, m.autoBotNames)
	return out
}

// HookRunSeq records the assigned run seq on each preflighted
// bot's pid file (so the live.jsonl tailer can decrement when
// the run finishes) and starts the project-event tailer. The
// tailer is idempotent across concurrent auto_bots runs in the
// same project.
//
// Returns the bots whose MarkAutoRun failed — typically the
// daemon crashed between WaitForReady and here so the pid file
// got reaped. Callers surface this as a non-fatal warning;
// the run is still created, just lacks auto-stop for those
// bots.
//
// projectDir is the project's local working directory — the
// supervisor reads <projectDir>/.enju/events/live.jsonl. Must
// be non-empty; an empty dir is logged + skipped (no tailer
// starts, every bot is reported as unhooked).
func (m *AutoRunManager) HookRunSeq(ctx context.Context, runSeq int64, projectDir string) []string {
	if len(m.autoBotNames) == 0 {
		return nil
	}
	var unhooked []string
	for _, name := range m.autoBotNames {
		if merr := m.sup.MarkAutoRun(name, runSeq); merr != nil {
			slog.Default().Warn("auto_run: MarkAutoRun failed", "bot", name, "run_seq", runSeq, "error", merr)
			unhooked = append(unhooked, name)
		}
	}
	m.sup.WatchProjectEvents(ctx, projectDir, m.projectID)
	return unhooked
}
