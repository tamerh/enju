package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/common/oplog"
)

// slogsink.go — the MCP process's slog destination.
//
// Why this exists: `enju mcp` talks to its client (Claude Code,
// etc.) over stdio; the client captures the subprocess's stderr
// and never shows it to the operator. A bare slog→os.Stderr
// handler is therefore a black hole — the supervisor /
// auto_bots / handler debug stream vanishes (this is what made
// the auto_stop bug take seven rounds to pin down).
//
// Scope rule (mirrors the oplog verb ledger in
// enjugit/workspace.go): logs about work on a project live under
// <project>/.enju/logs/; only host-global state lives in
// ~/.enju/. The MCP daemon is dormant and project-less until its
// first notifySession.Switch, so the slog can't be project-
// scoped at process start. It begins at a host-level bootstrap
// file and is re-pointed to
// <project>/.enju/logs/operator-slog-<pid>.log — a sibling of
// the oplog's operator-<pid>.log — the moment a project becomes
// active. A single MCP PID that switches projects re-points
// again, so each project's dir holds the slog for the window it
// was active (same per-project model the oplog already uses).
//
// Escape hatch: ENJU_MCP_LOG=stderr|stdout for the legacy
// streams, or an explicit path for a fixed file. Both pin the
// sink (no re-point) — the operator asked for that exact target.

// switchWriter is an io.Writer whose underlying target can be
// swapped at runtime. slog handler writes (any goroutine) and
// repoint (the notify Switch goroutine) race, so every access is
// mu-guarded. Cheap: one mutex, one pointer, no buffering of its
// own (slog's handler already frames whole records per Write).
type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	w := s.w
	s.mu.Unlock()
	return w.Write(p)
}

func (s *switchWriter) repoint(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

// setupMCPSlog builds the MCP process's slog logger and returns
// the optional project re-point hook (nil when the sink is
// pinned via ENJU_MCP_LOG). The hook is concurrency-safe and
// best-effort: a failed project-file open keeps the current
// writer rather than dropping logs or aborting the switch.
func setupMCPSlog() (*slog.Logger, func(projectDir string)) {
	mk := func(w io.Writer) *slog.Logger {
		return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelInfo}))
	}

	switch v := os.Getenv("ENJU_MCP_LOG"); v {
	case "stderr":
		return mk(os.Stderr), nil
	case "stdout":
		return mk(os.Stdout), nil
	case "":
		// Default: host-level bootstrap file, re-pointed per
		// project on Switch.
		home, err := os.UserHomeDir()
		if err != nil {
			fmt.Fprintf(os.Stderr, "MCP slog: no home dir (%v); using stderr\n", err)
			return mk(os.Stderr), nil
		}
		boot, err := oplog.OpenProjectLogFile(home, layout.LogsDir,
			oplog.TraceFilename("operator-slog-bootstrap"))
		if err != nil {
			fmt.Fprintf(os.Stderr, "MCP slog: bootstrap sink unavailable (%v); using stderr\n", err)
			return mk(os.Stderr), nil
		}
		sw := &switchWriter{w: boot}
		fmt.Fprintf(os.Stderr, "MCP slog → %s (re-points to <project>/.enju/logs/ on first project)\n", boot.Name())
		repoint := func(projectDir string) {
			if projectDir == "" {
				return
			}
			f, err := oplog.OpenProjectLogFile(projectDir, layout.LogsDir,
				oplog.TraceFilename("operator-slog"))
			if err != nil {
				// Keep the previous writer; a bad project dir
				// must not silence the daemon's own logs.
				fmt.Fprintf(os.Stderr, "MCP slog: project sink %s unavailable (%v); staying on previous\n", projectDir, err)
				return
			}
			// The previous file handle is intentionally left
			// open: closing it here would race a concurrent
			// slog Write that already read the old pointer.
			// Handles are bounded (1 bootstrap + 1 per distinct
			// active project in this session) and reclaimed on
			// process exit — a safe, tiny leak vs. a use-after-
			// close.
			sw.repoint(f)
			fmt.Fprintf(os.Stderr, "MCP slog → %s\n", f.Name())
		}
		return mk(sw), repoint
	default:
		// Explicit path: pinned, no re-point.
		f, err := os.OpenFile(expandPath(v), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
		if err != nil {
			fmt.Fprintf(os.Stderr, "MCP slog: cannot open %s (%v); using stderr\n", v, err)
			return mk(os.Stderr), nil
		}
		fmt.Fprintf(os.Stderr, "MCP slog → %s (pinned via ENJU_MCP_LOG)\n", f.Name())
		return mk(f), nil
	}
}
