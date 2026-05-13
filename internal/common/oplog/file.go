package oplog

import (
	"fmt"
	"os"
	"path/filepath"
)

// OpenProjectLogFile opens (creating if needed) an append-only
// log file at <workDir>/<subdir>/<filename>. Side effects on
// first use:
//
//   - mkdir -p <workDir>/<subdir>/
//   - drop a self-installing `.gitignore` containing "*\n" so the
//     directory's contents (including the .gitignore itself)
//     never end up in git. Same pattern internal/fatclient/notify
//     uses for .enju/events/.
//
// Concurrency model: each PROCESS picks a unique filename
// (typically with PID suffix — see ProcessTraceFilename) and
// writes to its own file. No cross-process append coordination
// needed. Within-process callers use the returned *os.File via
// slog.NewTextHandler or similar — slog handlers serialize
// writes internally, so multiple goroutines in the same process
// share the file safely without further locking.
//
// This avoids the POSIX-only O_APPEND atomicity assumption AND
// the complexity of cross-process flocking. The "single
// aggregated file across processes" view is built downstream:
// `tail -f <dir>/*.log` or a bot-side merger if richer queries
// are needed.
//
// Empty workDir or filename is a programming error and returns
// an error. Other setup failures (permission denied, disk full
// at mkdir) are returned to the caller.
func OpenProjectLogFile(workDir, subdir, filename string) (*os.File, error) {
	if workDir == "" {
		return nil, fmt.Errorf("oplog: OpenProjectLogFile: workDir is empty")
	}
	if filename == "" {
		return nil, fmt.Errorf("oplog: OpenProjectLogFile: filename is empty")
	}
	dir := filepath.Join(workDir, subdir)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("oplog: create %s: %w", dir, err)
	}
	if err := ensureSelfIgnoringGitignore(dir); err != nil {
		// Non-fatal: directory created OK; we'd rather have the
		// log file without the gitignore than nothing. Caller's
		// `git status` will show the file as untracked.
		_ = err
	}
	return os.OpenFile(filepath.Join(dir, filename),
		os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
}

// TraceFilename returns the per-process trace log filename for
// a given role. Format: `<role>-<pid>.log`. Role gives a human-
// readable handle ("operator", "bot-alice", "webui"); PID makes
// it unique across concurrent sessions or restarts of the same
// role. Empty role falls back to `trace-<pid>.log` so ad-hoc /
// test wirings still get a file.
//
// Discovery flow: `tail -f <projectRoot>/.enju/logs/*.log` for a
// live aggregate view; `cat operator-*.log` for every operator
// session's history; `ls -lt` to find the most recent.
//
// Each process owns its own file, so no cross-process write
// coordination is needed — within-process goroutines serialize
// via *os.File's runtime mutex, which is what slog handlers
// rely on too.
func TraceFilename(roleName string) string {
	if roleName == "" {
		return fmt.Sprintf("trace-%d.log", os.Getpid())
	}
	return fmt.Sprintf("%s-%d.log", roleName, os.Getpid())
}

// ensureSelfIgnoringGitignore writes a `.gitignore` containing
// `*\n` in dir if missing. The gitignore ignores everything in
// the directory including itself, so git never picks up anything
// from this tree. Idempotent.
func ensureSelfIgnoringGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	return os.WriteFile(path, []byte("*\n"), 0o644)
}
