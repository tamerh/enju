// Bot Supervisor — fatclient-side process manager for bot
// daemons.
//
// One Supervisor instance lives for the fatclient's lifetime
// and tracks every bot daemon the operator started through
// the MCP tools (enju_bot_start / _stop / _status / _logs).
// Each daemon runs as a child subprocess of the fatclient,
// stdin/stdout/stderr piped so the supervisor can:
//
//   - close stdin on Stop → trigger the daemon's
//     watchStdinEOF graceful shutdown (cross-platform stdlib
//     only; see cmd/enju/bot.go).
//   - capture log output to a per-bot file the operator
//     reads via enju_bot_logs.
//   - hard-kill via Process.Kill() when graceful timeout
//     expires.
//
// State scope: in-memory map keyed by bot name. PID files are
// written to disk for external diagnostic tools but the
// supervisor's authoritative state is its own map. Surviving
// a fatclient crash is NOT a goal for v1 — bot daemons started
// in a now-dead fatclient session become orphans that operators
// kill manually (the daemon's own stdin-EOF read fails when
// the parent's pipe closes, so they shut down cleanly on
// crash unless they were mid-LLM-call).

package bots

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Supervisor manages bot daemon subprocesses.
type Supervisor struct {
	// EnjuExec is the path to the `enju` binary the supervisor
	// invokes for `enju bot run`. Defaults to os.Executable()
	// at construction so the supervisor invokes the same
	// binary that's running it (no PATH ambiguity, no
	// version skew between supervisor and daemon).
	EnjuExec string

	// PIDDir is where per-bot PID files land. Default:
	// ~/.enju/bots/pids. Each entry is botname.json so an
	// external diagnostic tool can answer "is developer-bot
	// running, and on what PID?" without consulting the
	// fatclient.
	PIDDir string

	// LogDir is where per-bot stdout+stderr capture lands.
	// Default: ~/.enju/bots/logs. One file per bot
	// (botname.log), opened append-mode so successive starts
	// of the same bot keep history (operators reading logs
	// after a crash see what was happening before).
	LogDir string

	// GracefulTimeout caps how long Stop waits after closing
	// stdin before falling back to hard-kill. Default 5s —
	// enough for the daemon to release its claim and exit
	// cleanly, short enough that operators don't think
	// something hung.
	GracefulTimeout time.Duration

	Logger *slog.Logger

	// procsMu guards procs (the in-memory tracking map) AND
	// recentExits (the ring buffer). Concurrent MCP tool
	// calls — Start + Stop on different bots, Status walking
	// the map while Start writes — all go through this lock.
	procsMu     sync.Mutex
	procs       map[string]*botProcess
	recentExits []ExitEvent // bounded ring; see recentExitsMax
}

// recentExitsMax bounds the in-memory exit-event ring buffer.
// Deliberately small: the ring's purpose is "did my bot just
// crash?" — a few entries is enough. Operators wanting deeper
// history read enju_bot_logs.
const recentExitsMax = 16

// ExitEvent records a bot daemon's exit for the recently-
// exited surface. Surfaced by Status so an operator who asks
// "what happened to reviewer-bot?" sees a result instead of
// silence (the previous behavior was to delete the entry on
// reaper run, leaving no trace).
type ExitEvent struct {
	Name     string    `json:"name"`
	PID      int       `json:"pid"`
	ExitedAt time.Time `json:"exited_at"`
	// Reason is "graceful" | "hard-killed" | "crashed:<msg>".
	// Free-text rather than enum so future signal-paths can
	// land here without a schema bump.
	Reason  string `json:"reason"`
	LogPath string `json:"log_path"`
}

// logger returns a non-nil logger so callers don't have to
// remember to set Supervisor.Logger.
func (s *Supervisor) logger() *slog.Logger {
	if s.Logger != nil {
		return s.Logger
	}
	return slog.Default()
}

// botProcess is the supervisor's per-daemon state. Held only
// in memory; the PID file on disk is the authoritative source
// for tools that don't have access to the supervisor instance.
type botProcess struct {
	Name      string
	Cmd       *exec.Cmd
	Stdin     io.WriteCloser // close this to trigger graceful shutdown
	StartedAt time.Time
	LogPath   string
	PIDPath   string

	// hardKilled is set by Stop when the graceful timeout
	// expired and we had to fall back to Process.Kill. Read
	// by reapOnExit when classifying the exit reason.
	// Guarded by Supervisor.procsMu since Stop and reap run
	// on different goroutines.
	hardKilled bool
}

// NewSupervisor returns a Supervisor with sensible defaults.
// Caller can override fields before first use.
//
// Side effect: prunes stale PID files from a previous fatclient
// session whose processes are no longer alive. Surviving a
// fatclient crash isn't a goal (orphaned daemons are killed
// manually by the operator) but leaving stale PID files around
// would mislead external diagnostic tools that read the
// directory. Best-effort: errors are logged, not fatal.
func NewSupervisor() (*Supervisor, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating enju binary: %w", err)
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return nil, fmt.Errorf("resolving home directory: %w", err)
	}
	s := &Supervisor{
		EnjuExec:        exe,
		PIDDir:          filepath.Join(home, ".enju", "bots", "pids"),
		LogDir:          filepath.Join(home, ".enju", "bots", "logs"),
		GracefulTimeout: 5 * time.Second,
		Logger:          slog.Default(),
		procs:           make(map[string]*botProcess),
	}
	s.pruneStalePIDFiles()
	return s, nil
}

// pruneStalePIDFiles walks PIDDir and removes entries whose
// process is no longer running. Identifies "alive" via
// os.FindProcess + signal-0 (sending zero to a process is
// portable; succeeds when the process exists, errors when it
// doesn't). Called from NewSupervisor so a fresh fatclient
// session starts with an authoritative PID directory.
func (s *Supervisor) pruneStalePIDFiles() {
	entries, err := os.ReadDir(s.PIDDir)
	if err != nil {
		// Directory doesn't exist yet (first-ever supervisor)
		// or unreadable — nothing to prune. Not an error.
		return
	}
	for _, ent := range entries {
		if ent.IsDir() {
			continue
		}
		path := filepath.Join(s.PIDDir, ent.Name())
		raw, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		var entry pidFileEntry
		if err := json.Unmarshal(raw, &entry); err != nil {
			// Malformed entry — drop it; it's worse than
			// useless to a diagnostic tool.
			_ = os.Remove(path)
			continue
		}
		if entry.PID <= 0 {
			_ = os.Remove(path)
			continue
		}
		proc, err := os.FindProcess(entry.PID)
		if err != nil {
			_ = os.Remove(path)
			continue
		}
		// signal 0: portable "is the process there?" probe.
		// On Unix, returns nil if alive, error if not. On
		// Windows, FindProcess always returns a Process so
		// signal(0) always succeeds — at the cost that we
		// can't detect dead processes there. Acceptable for
		// v1: the worst case is a stale entry survives,
		// which is the pre-fix behavior anyway.
		if err := proc.Signal(syscall.Signal(0)); err != nil {
			s.logger().Info("pruning stale pid file", "name", entry.Name, "pid", entry.PID)
			_ = os.Remove(path)
		}
	}
}

// StartParams is the input shape for Supervisor.Start. Mirrors
// the cmdBotRun CLI flags so the MCP tool handler can pass
// everything through.
type StartParams struct {
	BotName     string // required
	ProjectDir  string // required, absolute
	Coordinator string // required (e.g. http://localhost:8000)
	ProjectID   int64  // optional (0 = across all projects bot is a member of)

	// AllowTools, when non-empty, is forwarded to the daemon
	// as --allow-tools=tool1,tool2,... — the daemon passes
	// it on to the MCP host the LLM eventually talks to (in
	// the Phase 2.4+ action=contribute path; v1 review/vote
	// actions don't invoke MCP tools so the allowlist is
	// declarative-only for them). Sourced from the bot's
	// manifest mcp_tools.allow at handleBotStart-time so the
	// trust-model story (manifest declares → runner pins →
	// audit log records) is wired end-to-end now even if the
	// pinning is a no-op for current action types.
	AllowTools []string
}

// RunningBot is the public-facing per-bot status tuple.
type RunningBot struct {
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	LogPath   string    `json:"log_path"`
}

// pidFileEntry is what we write to disk per bot. Plain JSON
// for grep-ability; readable by `cat ~/.enju/bots/pids/x.json`.
type pidFileEntry struct {
	Name      string    `json:"name"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	LogPath   string    `json:"log_path"`
}

// Start spawns a new bot daemon. Returns an error if a daemon
// for that bot is already running (single-bot-per-machine
// invariant for v1; multi-instance is post-launch work).
//
// The daemon runs as `enju bot run --bot=<name> --project=<dir>
// --coordinator=<url>` with stdin connected so Stop can close
// it for graceful shutdown. Stdout/stderr go to the per-bot
// log file, opened append-mode.
func (s *Supervisor) Start(ctx context.Context, p StartParams) (*RunningBot, error) {
	if p.BotName == "" || p.ProjectDir == "" || p.Coordinator == "" {
		return nil, fmt.Errorf("Start: bot_name, project_dir, and coordinator are required")
	}
	s.procsMu.Lock()
	if _, exists := s.procs[p.BotName]; exists {
		s.procsMu.Unlock()
		return nil, fmt.Errorf("bot %q is already running — call enju_bot_stop first", p.BotName)
	}
	s.procsMu.Unlock()

	if err := os.MkdirAll(s.PIDDir, 0700); err != nil {
		return nil, fmt.Errorf("preparing pid dir: %w", err)
	}
	// MkdirAll only sets mode on creation. If the dir
	// already exists at 0755 from a prior tool, bot PID
	// files at 0600 inside a 0755 dir leak filenames (which
	// include bot names) via directory traversal. Same
	// hardening pattern as Phase 1's credentials directory.
	if err := os.Chmod(s.PIDDir, 0700); err != nil {
		return nil, fmt.Errorf("tightening pid dir mode: %w", err)
	}
	if err := os.MkdirAll(s.LogDir, 0700); err != nil {
		return nil, fmt.Errorf("preparing log dir: %w", err)
	}
	if err := os.Chmod(s.LogDir, 0700); err != nil {
		return nil, fmt.Errorf("tightening log dir mode: %w", err)
	}
	logPath := filepath.Join(s.LogDir, p.BotName+".log")
	pidPath := filepath.Join(s.PIDDir, p.BotName+".json")

	// O_APPEND: successive starts of the same bot keep the
	// log history. Useful when a bot crashes; the operator
	// reading enju_bot_logs after a restart sees the lead-up.
	logFile, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		return nil, fmt.Errorf("opening log file: %w", err)
	}

	args := []string{
		"bot", "run",
		"--bot=" + p.BotName,
		"--project=" + p.ProjectDir,
		"--coordinator=" + p.Coordinator,
	}
	if p.ProjectID > 0 {
		args = append(args, fmt.Sprintf("--project-id=%d", p.ProjectID))
	}
	if len(p.AllowTools) > 0 {
		args = append(args, "--allow-tools="+strings.Join(p.AllowTools, ","))
	}
	cmd := exec.Command(s.EnjuExec, args...)
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = logFile.Close()
		return nil, fmt.Errorf("opening stdin pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		_ = stdin.Close()
		_ = logFile.Close()
		return nil, fmt.Errorf("spawning bot daemon: %w", err)
	}

	now := time.Now()
	bp := &botProcess{
		Name:      p.BotName,
		Cmd:       cmd,
		Stdin:     stdin,
		StartedAt: now,
		LogPath:   logPath,
		PIDPath:   pidPath,
	}
	s.procsMu.Lock()
	s.procs[p.BotName] = bp
	s.procsMu.Unlock()

	if err := writePIDFile(pidPath, pidFileEntry{
		Name: p.BotName, PID: cmd.Process.Pid, StartedAt: now, LogPath: logPath,
	}); err != nil {
		// Non-fatal: the supervisor still has the process in
		// memory. PID file is for external diagnostics.
		s.logger().Warn("writing pid file", "bot", p.BotName, "error", err)
	}

	// Reaper goroutine: when cmd.Wait returns the process
	// exited (graceful or crash). Clean up our tracking so a
	// later Start can re-launch it. logFile closes in here
	// so Wait can release its FD even on crash paths.
	go s.reapOnExit(bp, logFile)

	return &RunningBot{
		Name:      p.BotName,
		PID:       cmd.Process.Pid,
		StartedAt: now,
		LogPath:   logPath,
	}, nil
}

// StopResult tells callers HOW the daemon exited. graceful=true
// means the daemon honored stdin-EOF and exited within
// GracefulTimeout; graceful=false means we had to hard-kill.
// The MCP tool surface uses this so the operator distinguishes
// "shut down cleanly" from "had to be force-killed" — the
// latter is interesting because it means the daemon was
// holding open work it couldn't release.
type StopResult struct {
	Graceful bool `json:"graceful"`
}

// Stop closes the daemon's stdin to trigger graceful shutdown,
// then waits up to GracefulTimeout. If the process is still
// alive after the timeout, falls back to Process.Kill() (hard
// kill). Idempotent: stopping an unknown bot returns a clear
// "not running" error.
//
// The supervisor's reaper goroutine handles cleanup of the
// in-memory entry + PID file once the process exits, so this
// method just initiates shutdown.
func (s *Supervisor) Stop(ctx context.Context, botName string) (StopResult, error) {
	s.procsMu.Lock()
	bp, ok := s.procs[botName]
	s.procsMu.Unlock()
	if !ok {
		return StopResult{}, fmt.Errorf("bot %q is not running (or wasn't started by this supervisor session)", botName)
	}
	// Closing stdin triggers the daemon's watchStdinEOF
	// goroutine, which cancels its ctx and lets the runner's
	// deferred ReleaseActiveClaim fire. Cross-platform.
	if err := bp.Stdin.Close(); err != nil {
		s.logger().Warn("closing daemon stdin", "bot", botName, "error", err)
	}

	// Wait for graceful exit. processExited polls every
	// 50ms because cmd.Wait is consumed by the reaper
	// goroutine — we can't double-Wait. ProcessState lookup
	// via os.FindProcess + signal-0 isn't fully cross-
	// platform, so we lean on the reaper having cleared the
	// map entry as the "process exited" signal.
	deadline := time.Now().Add(s.GracefulTimeout)
	for time.Now().Before(deadline) {
		s.procsMu.Lock()
		_, stillTracked := s.procs[botName]
		s.procsMu.Unlock()
		if !stillTracked {
			return StopResult{Graceful: true}, nil
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			return StopResult{}, ctx.Err()
		}
	}

	// Graceful timeout exceeded — hard kill. Mark on the
	// botProcess so the reaper can record the exit reason
	// when cmd.Wait eventually returns. The reaper still
	// cleans up the map entry once cmd.Wait returns (which
	// it will after Process.Kill).
	s.logger().Warn("graceful timeout exceeded; hard-killing", "bot", botName, "timeout", s.GracefulTimeout)
	s.procsMu.Lock()
	bp.hardKilled = true
	s.procsMu.Unlock()
	if err := bp.Cmd.Process.Kill(); err != nil {
		return StopResult{}, fmt.Errorf("hard-kill of bot %q: %w", botName, err)
	}
	return StopResult{Graceful: false}, nil
}

// Status returns every bot the supervisor is currently tracking.
// Order is alphabetical by name for stable output. Excludes
// processes that exited but whose reaper hasn't run yet
// (reapOnExit clears the map entry before the next reader
// sees it; race window is ms-scale and benign — caller will
// see the bot as "not running" momentarily).
func (s *Supervisor) Status() []RunningBot {
	s.procsMu.Lock()
	defer s.procsMu.Unlock()
	names := make([]string, 0, len(s.procs))
	for n := range s.procs {
		names = append(names, n)
	}
	sortStrings(names)
	out := make([]RunningBot, 0, len(names))
	for _, n := range names {
		bp := s.procs[n]
		out = append(out, RunningBot{
			Name:      bp.Name,
			PID:       bp.Cmd.Process.Pid,
			StartedAt: bp.StartedAt,
			LogPath:   bp.LogPath,
		})
	}
	return out
}

// Logs returns the last `lines` lines of the bot's log file.
// If the bot was never started (or the log file doesn't yet
// exist), returns an empty slice. lines<=0 returns the entire
// log (with a soft cap of 10000 to keep MCP responses bounded).
func (s *Supervisor) Logs(botName string, lines int) ([]string, error) {
	logPath := filepath.Join(s.LogDir, botName+".log")
	f, err := os.Open(logPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("opening log file: %w", err)
	}
	defer f.Close()
	if lines <= 0 || lines > 10000 {
		lines = 10000
	}
	// Naive ring-buffer tail: scan all lines, keep the last N.
	// Logs are bounded enough in practice that we don't need
	// reverse-seek heroics. If a daemon ever produces gigantic
	// logs, the soft cap above plus the per-bot log-rotation
	// future work covers it.
	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)
	ring := make([]string, 0, lines)
	for scanner.Scan() {
		if len(ring) == lines {
			ring = append(ring[1:], scanner.Text())
		} else {
			ring = append(ring, scanner.Text())
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("scanning log file: %w", err)
	}
	return ring, nil
}

// StopAll attempts to gracefully stop every running bot.
// Errors from individual stops are collected but don't
// short-circuit the loop — best-effort cleanup is more useful
// than failing fast when half the fleet is already down.
//
// Useful at fatclient shutdown so the operator's processes
// don't outlive their parent.
func (s *Supervisor) StopAll(ctx context.Context) []error {
	s.procsMu.Lock()
	names := make([]string, 0, len(s.procs))
	for n := range s.procs {
		names = append(names, n)
	}
	s.procsMu.Unlock()

	var errs []error
	for _, n := range names {
		if _, err := s.Stop(ctx, n); err != nil {
			errs = append(errs, fmt.Errorf("stop %s: %w", n, err))
		}
	}
	return errs
}

// reapOnExit waits for the daemon to exit, then cleans up the
// supervisor's in-memory tracking + PID file. Runs in its own
// goroutine per bot, started by Start. Pushes an ExitEvent into
// the recently-exited ring so Status surfaces "your bot
// crashed 5s ago" instead of silently empty.
//
// logFile parameter is the file handle Start opened — closing
// it here releases the FD whether the daemon exited gracefully
// or crashed.
func (s *Supervisor) reapOnExit(bp *botProcess, logFile *os.File) {
	waitErr := bp.Cmd.Wait()
	_ = logFile.Close()

	s.procsMu.Lock()
	hardKilled := bp.hardKilled
	delete(s.procs, bp.Name)
	// Classify the exit reason. graceful when Wait returned
	// nil and we didn't have to hard-kill; hard-killed when
	// Stop forced it; crashed otherwise (non-zero exit
	// without operator intervention).
	reason := "graceful"
	switch {
	case hardKilled:
		reason = "hard-killed"
	case waitErr != nil:
		reason = "crashed: " + waitErr.Error()
	}
	s.recentExits = append(s.recentExits, ExitEvent{
		Name:     bp.Name,
		PID:      bp.Cmd.Process.Pid,
		ExitedAt: time.Now(),
		Reason:   reason,
		LogPath:  bp.LogPath,
	})
	if len(s.recentExits) > recentExitsMax {
		s.recentExits = s.recentExits[len(s.recentExits)-recentExitsMax:]
	}
	s.procsMu.Unlock()

	if waitErr != nil {
		s.logger().Info("bot daemon exited", "bot", bp.Name, "reason", reason, "exit_error", waitErr)
	} else {
		s.logger().Info("bot daemon exited cleanly", "bot", bp.Name, "reason", reason)
	}

	if err := os.Remove(bp.PIDPath); err != nil && !os.IsNotExist(err) {
		s.logger().Warn("removing pid file", "bot", bp.Name, "error", err)
	}
}

// RecentExits returns a copy of the recently-exited ring, oldest
// first. Used by Status to surface "your bot crashed N seconds
// ago" so operators don't see an empty status response after
// a crash. Bounded to recentExitsMax entries.
func (s *Supervisor) RecentExits() []ExitEvent {
	s.procsMu.Lock()
	defer s.procsMu.Unlock()
	out := make([]ExitEvent, len(s.recentExits))
	copy(out, s.recentExits)
	return out
}

// writePIDFile serializes pidFileEntry as JSON. 0600 since the
// file lives under ~/.enju/bots/pids and shares the same
// privacy posture as the credentials directory.
func writePIDFile(path string, e pidFileEntry) error {
	data, err := json.MarshalIndent(e, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, data, 0600)
}

// sortStrings is a tiny insertion-sort helper. Status results
// are tiny (single-digit count of bots) — sort.Strings is
// overkill but pulling in the import for one call site is
// hygienic; using insertion sort here keeps the supervisor
// import list smaller.
func sortStrings(xs []string) {
	for i := 1; i < len(xs); i++ {
		for j := i; j > 0 && xs[j-1] > xs[j]; j-- {
			xs[j-1], xs[j] = xs[j], xs[j-1]
		}
	}
}

