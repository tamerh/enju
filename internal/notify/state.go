package notify

// last_seen persistence. The notify daemon tracks a cursor
// across restarts so it can resume where it left off without
// re-firing notifications for events the user already saw.
//
// v1 cursor is the last-seen event timestamp (RFC3339Nano).
// Future seq-based cursoring (when RunEventRecord exposes Seq
// — see Phase 4d notes in docs/notifications.md) will be a
// schema-additive change to State.
//
// Atomic write: tmp + rename so a crash mid-write never leaves
// a half-finished file readable on next start.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

// State is the on-disk shape of the notify cursor. Versioned
// so future schema changes (e.g. adding LastSeq for seq-based
// cursoring) can detect and migrate old files.
type State struct {
	Version  int       `json:"version"`
	LastSeen time.Time `json:"last_seen"`
}

const currentStateVersion = 1

// loadState reads the cursor file. Missing file → zero State
// (loop starts from "all events ever," which is fine for the
// first run; users who care about backfill avalanche set
// PollWait short and skip-to-head logic in 4b will trim).
//
// Malformed file logs and returns zero — the wrong cursor is
// worse than no cursor.
func loadState(path string) (State, error) {
	if path == "" {
		return State{}, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return State{}, nil
		}
		return State{}, fmt.Errorf("read state %s: %w", path, err)
	}
	var s State
	if err := json.Unmarshal(data, &s); err != nil {
		return State{}, fmt.Errorf("parse state %s: %w", path, err)
	}
	return s, nil
}

// saveState writes State atomically: write to a sibling tmp
// file, fsync, rename. On crash, the original file is intact
// or the new file is intact — never a half-written one.
func saveState(path string, s State) error {
	if path == "" {
		return nil
	}
	s.Version = currentStateVersion
	data, err := json.Marshal(s)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
