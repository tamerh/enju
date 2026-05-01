package notify

// Notify cursor persistence. Tracks a per-project resume point
// across MCP restarts so the loop doesn't re-fire notifications
// for events the user already saw. seq-based — the per-project
// monotone integer that /events?since_seq=N filters strictly
// `>` against. No timestamp involved.
//
// Atomic write: tmp + rename so a crash mid-write never leaves
// a half-finished file readable on next start.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// State is the on-disk shape of the notify cursor. Versioned so
// future schema changes can detect and skip mismatched files
// safely.
type State struct {
	Version int   `json:"version"`
	LastSeq int64 `json:"last_seq"`
}

const currentStateVersion = 1

// loadState reads the cursor file. Missing file → zero State
// (start from seq=0, which the strict-`>` filter on the server
// resolves to "every event in the project"). Malformed file
// returns zero — wrong cursor is worse than no cursor.
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
// file, rename. On crash, the original file is intact or the new
// file is intact — never a half-written one.
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
