package mcpserver

// notifySession manages the per-MCP-session notification poller.
// One active project at a time per process. When a tool call
// signals "the user is working on project X" (create_project,
// init), Switch cancels the previous goroutine and spins up a
// new one for X.
//
// All persistent state is project-scoped: the cursor lives at
// {project_clone}/enju/events/cursor.json, the live event log at
// {project_clone}/enju/events/live.jsonl, the user's Layer 3
// rules at {project_clone}/enju/notify.yaml. There is no cross-
// session record of "which project was active last time" — a
// fresh MCP boot stays dormant until the user touches a project.
//
// Lifecycle:
//
//   - Boot: cmdMCP creates the session with workspace + ctx +
//     credentials. The session waits idle.
//   - During a session: Switch fires from project handlers and
//     starts polling.
//   - Shutdown: parent ctx cancellation cascades into the
//     session's child contexts; the notify goroutine drains.

import (
	"context"
	"log/slog"
	"sync"

	"github.com/enju-ai/enju/internal/fatclient/mcpgit"
	"github.com/enju-ai/enju/internal/fatclient/notify"
)

// notifySessionConfig is the boot-time wiring the session needs
// to spawn notify.Run for any project. Resolved by mcpserver.New
// from Config.Notify.
type notifySessionConfig struct {
	CoordinatorURL string
	// TokenFn returns the live bearer token. Critical for the
	// "coordinator DB wiped, citizen re-registered" recovery
	// path: apiClient's auto-reregister updates its atomic.Value
	// token, this getter reads from there, the next notify poll
	// uses the fresh token. No MCP restart required.
	TokenFn   func() string
	Username  string
	Workspace *mcpgit.Workspace // resolves project clone dirs
	ParentCtx context.Context   // cancels every child goroutine on shutdown
	Logger    *slog.Logger
}

// notifySession is the runtime handle. Methods are safe for
// concurrent calls.
type notifySession struct {
	cfg notifySessionConfig

	mu        sync.Mutex
	cancel    context.CancelFunc // cancels the active goroutine
	projectID int64              // 0 when nothing is running
}

func newNotifySession(cfg notifySessionConfig) *notifySession {
	return &notifySession{cfg: cfg}
}

// Switch makes projectID the active notify target. Cancels the
// prior goroutine (if any) and spawns a new one. No-op when
// projectID matches the current active. Best-effort: errors
// resolving the project's clone dir or loading user rules are
// logged but never block the switch — notification delivery is
// observable data, not state.
//
// projectID == 0 stops notify entirely (used for explicit clear).
func (s *notifySession) Switch(projectID int64) {
	if s == nil || s.cfg.ParentCtx == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if projectID == s.projectID {
		return
	}
	if s.cancel != nil {
		s.cancel()
		s.cancel = nil
	}
	s.projectID = projectID
	if projectID == 0 {
		return
	}

	projectDir := ""
	if s.cfg.Workspace != nil {
		projectDir = s.cfg.Workspace.ProjectDir(projectID)
	}
	if projectDir == "" {
		// No clone yet — happens between project creation and
		// first workspace touch. Notify still runs but cursor +
		// live.jsonl writes skip silently when path is empty.
		s.cfg.Logger.Info("notify: no local clone for project, running with no on-disk state",
			"project_id", projectID)
	}

	runCfg := notify.Config{
		CoordinatorURL: s.cfg.CoordinatorURL,
		ProjectID:      projectID,
		Username:       s.cfg.Username,
		BearerTokenFn:  s.cfg.TokenFn,
		ProjectDir:     projectDir,
		Logger:         s.cfg.Logger,
	}

	ctx, cancel := context.WithCancel(s.cfg.ParentCtx)
	s.cancel = cancel
	go func() {
		s.cfg.Logger.Info("notify: poller started for project",
			"project_id", projectID, "project_dir", projectDir)
		if err := notify.Run(ctx, runCfg); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Error("notify: poller exited with error",
				"project_id", projectID, "err", err)
		}
	}()
}
