package notify

// Append-only JSONL event log. The local substrate that future
// Tier 2 bots tail to consume events without polling the
// coordinator independently. v1 writes here as a side-effect of
// the in-process dispatch; the file is the canonical local
// record once Tier 2 ships.
//
// Format: one JSON object per line, seq-ordered. Lines use the
// same wire shape /events serves (seq + ts + type + ... +
// metadata), so a `cat live.jsonl | jq` view matches a `curl
// /events` view.
//
// Concurrency: O_APPEND makes single-line writes atomic on POSIX
// for sizes < PIPE_BUF (4096 bytes). Notification events are
// always small. No locking needed even if multiple writers (a
// future "merge two coordinators' streams" tool, etc.) point at
// the same file.
//
// Rotation: not in v1. The file grows. When it gets large,
// rotate by `mv live.jsonl archive-{seqlo}-{seqhi}.jsonl &&
// touch live.jsonl`. Bots tracking by seq cross the boundary
// cleanly because seq is monotone.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// appendEventToLog appends one event as a JSON line to the
// project's live.jsonl. Returns nil silently when no path is
// configured (tests that don't set ProjectDir).
//
// Best-effort: write failures are logged by the caller and the
// notify loop continues. Missing parent dir is created lazily,
// along with a self-installing .gitignore so the events dir's
// contents (live.jsonl, cursor.json, future archives) never
// land in git.
func appendEventToLog(path string, ev Event) error {
	if path == "" {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("create events dir: %w", err)
	}
	if err := ensureGitignore(dir); err != nil {
		// Non-fatal — log path is the priority. Caller logs
		// the warning if it cares.
		return fmt.Errorf("ensure gitignore in %s: %w", dir, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer f.Close()
	line, err := json.Marshal(ev)
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	line = append(line, '\n')
	if _, err := f.Write(line); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// ensureGitignore writes a self-installing `.gitignore` in the
// events dir if missing. Content is "*\n" — ignores everything
// in the directory including the .gitignore itself, so git never
// commits anything from this tree. Each clone independently
// creates its own .gitignore on first notify write; nothing
// crosses repo boundaries.
//
// Idempotent — does nothing if the file already exists.
func ensureGitignore(dir string) error {
	path := filepath.Join(dir, ".gitignore")
	if _, err := os.Stat(path); err == nil {
		return nil // already there
	}
	return os.WriteFile(path, []byte("*\n"), 0o644)
}
