package oplog

import (
	"fmt"
	"os"
	"path/filepath"
)

// OpenProjectLogFile opens (creating if needed) an append-only log
// file at <workDir>/<subdir>/<filename>. Side effects on first
// use:
//
//   - mkdir -p <workDir>/<subdir>/
//   - drop a self-installing `.gitignore` containing "*\n" so the
//     directory's contents (including the .gitignore itself) never
//     end up in git. Same pattern internal/fatclient/notify uses
//     for enju/events/.
//
// The returned file is opened O_APPEND|O_CREATE|O_WRONLY so
// concurrent processes (operator MCP + multiple bot daemons,
// notify supervisor + writer goroutines) can each append safely
// on Linux without coordination.
//
// Empty workDir is a programming error and returns an error.
// Other setup failures (permission denied, disk full at mkdir)
// are returned to the caller, who decides whether they're fatal
// (notify treats them as fatal — events would be lost) or
// degradation-acceptable (oplog treats them as fall-through to
// the slog logger).
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
