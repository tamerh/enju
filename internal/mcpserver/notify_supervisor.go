package mcpserver

// notifySupervisor manages the per-MCP-session notification
// poller. One active project at a time: when a tool call signals
// "the user is working on project X" (create_project, init), the
// supervisor cancels the previous notify goroutine and spins up
// a new one for X. The active project is persisted so the next
// `enju mcp` restart resumes on the same project without forcing
// the user to re-enable.
//
// Lifecycle:
//
//   - Boot: cmdMCP creates the supervisor with the parameters
//     notify.Run needs (coordinator URL, token, username, config
//     paths). If a saved active-project exists for this
//     coordinator, supervisor.Switch fires immediately.
//   - During a session: project handlers (handleCreateProject,
//     handleInit) call Switch with the new project's ID.
//   - Shutdown: when ServeStdio returns, cmdMCP cancels the
//     parent context, which cancels the supervisor's child
//     contexts and exits the notify goroutine cleanly.

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/enju-ai/enju/internal/notify"
)

// notifySupervisorConfig is the boot-time config the supervisor
// needs to spawn notify.Run. Held by reference so callers can
// build it once and pass it through mcpserver.Config.
type notifySupervisorConfig struct {
	// CoordinatorURL is forwarded to notify.Config.CoordinatorURL.
	CoordinatorURL string
	// BearerToken authenticates the long-poll requests.
	BearerToken string
	// Username is the running citizen's handle, used for {{me}}
	// resolution in default rules and templates.
	Username string
	// CoordinatorKey is the credentials-keyed coordinator id
	// ("local" for embedded mode, the URL otherwise). Used to
	// scope active-project persistence per coordinator.
	CoordinatorKey string
	// UserConfigPath is the Layer 3 user-rules YAML path
	// (typically ~/.enju/notify.yaml).
	UserConfigPath string
	// StateFileFunc returns the per-project cursor state file
	// (typically ~/.enju/notify-state-{projectID}.json). Func so
	// the supervisor can compute it lazily per Switch call.
	StateFileFunc func(projectID int64) string
	// ActiveProjectPath is the cross-restart hint file
	// (~/.enju/notify-active.json).
	ActiveProjectPath string
	// ParentCtx scopes every notify goroutine the supervisor
	// spawns — cancelled on MCP shutdown to drain cleanly.
	ParentCtx context.Context
	// Logger receives diagnostic output for the supervisor itself
	// + the notify goroutines it spawns.
	Logger *slog.Logger
}

// notifySupervisor is the runtime handle. Constructed once,
// methods are safe for concurrent calls.
type notifySupervisor struct {
	cfg notifySupervisorConfig

	mu        sync.Mutex
	cancel    context.CancelFunc // cancels the active goroutine
	projectID int64              // 0 when nothing is running
}

func newNotifySupervisor(cfg notifySupervisorConfig) *notifySupervisor {
	return &notifySupervisor{cfg: cfg}
}

// Switch makes projectID the active notify target. Cancels the
// prior goroutine (if any) and spawns a new one. No-op when
// projectID matches the current active. Best-effort: errors
// loading user config or persisting active-project are logged
// but never block the switch — notification delivery is observable
// data, not state.
//
// projectID == 0 stops notify entirely (used for explicit clear).
func (s *notifySupervisor) Switch(projectID int64) {
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
		_ = notify.SaveActiveProject(s.cfg.ActiveProjectPath, s.cfg.CoordinatorKey, 0)
		return
	}

	uc, warnings, err := notify.LoadUserConfig(s.cfg.UserConfigPath)
	if err != nil {
		s.cfg.Logger.Warn("notify: failed to load user config — continuing with Layer 1 defaults only",
			"path", s.cfg.UserConfigPath, "err", err)
		uc = notify.UserConfig{}
	}
	for _, w := range warnings {
		s.cfg.Logger.Warn("notify: config issue", "path", s.cfg.UserConfigPath, "issue", w)
	}

	stateFile := ""
	if s.cfg.StateFileFunc != nil {
		stateFile = s.cfg.StateFileFunc(projectID)
	}

	runCfg := notify.Config{
		CoordinatorURL:  s.cfg.CoordinatorURL,
		ProjectID:       projectID,
		Username:        s.cfg.Username,
		BearerToken:     s.cfg.BearerToken,
		Rules:           uc.ToRules(),
		DisableDefaults: uc.DisableDefaults,
		StateFile:       stateFile,
		Logger:          s.cfg.Logger,
	}

	ctx, cancel := context.WithCancel(s.cfg.ParentCtx)
	s.cancel = cancel
	go func() {
		s.cfg.Logger.Info("notify: poller started for project",
			"project_id", projectID, "rules", len(runCfg.Rules))
		if err := notify.Run(ctx, runCfg); err != nil && ctx.Err() == nil {
			s.cfg.Logger.Error("notify: poller exited with error",
				"project_id", projectID, "err", err)
		}
	}()

	if err := notify.SaveActiveProject(s.cfg.ActiveProjectPath, s.cfg.CoordinatorKey, projectID); err != nil {
		s.cfg.Logger.Warn("notify: failed to persist active project (will not auto-resume on restart)",
			"err", err)
	}
}

// defaultNotifyStateFileFunc builds the per-project cursor path
// under the user's enju home dir. Returned func gracefully
// degrades to "" when the home dir is unavailable, matching the
// rest of the notify package's "missing path = no persistence"
// convention.
func defaultNotifyStateFileFunc(homeDir string) func(int64) string {
	return func(projectID int64) string {
		if homeDir == "" {
			return ""
		}
		return filepath.Join(homeDir, ".enju", fmt.Sprintf("notify-state-%d.json", projectID))
	}
}
