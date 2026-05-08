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
// Concurrency: writers are within a single process today (the
// notify supervisor's in-process dispatch). The *os.File handle
// + Go's runtime serialize concurrent goroutine writes; no
// further locking needed. If notify ever grows multi-process
// writers (e.g. bot daemons writing into the operator's stream
// directly), this needs revisiting — Windows in particular
// doesn't guarantee O_APPEND atomicity across processes.
//
// Rotation: not in v1. The file grows. When it gets large,
// rotate by `mv live.jsonl archive-{seqlo}-{seqhi}.jsonl &&
// touch live.jsonl`. Bots tracking by seq cross the boundary
// cleanly because seq is monotone.
//
// File-sink mechanics (mkdir + self-installing .gitignore +
// O_APPEND open) live in internal/common/oplog. enjugit's
// trace.log uses the same primitive so the project layout's
// conventions for "untracked append-only logs" are encoded once.

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/enju-ai/enju/internal/common/oplog"
)

// appendEventToLog appends one event as a JSON line to the
// project's live.jsonl. Returns nil silently when no path is
// configured (tests that don't set ProjectDir).
//
// Best-effort: write failures are logged by the caller and the
// notify loop continues. The file's parent dir is created lazily
// (with self-installing .gitignore via oplog.OpenProjectLogFile)
// so the events dir's contents never land in git.
func appendEventToLog(path string, ev Event) error {
	if path == "" {
		return nil
	}
	// Split path into <workDir>/<subdir>/<filename> for the
	// oplog primitive. notify configures `path` as
	// "<projectDir>/enju/events/live.jsonl"; we hand each part
	// in separately so oplog can mkdir the right places and
	// drop a .gitignore in <projectDir>/enju/events/.
	dir := filepath.Dir(path)
	parent := filepath.Dir(dir)
	subdir := filepath.Base(dir)
	filename := filepath.Base(path)
	f, err := oplog.OpenProjectLogFile(parent, subdir, filename)
	if err != nil {
		return fmt.Errorf("open events log %s: %w", path, err)
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
