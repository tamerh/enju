package store

import (
	"encoding/json"
	"strings"
)

// SyncStatusKind enumerates the run-completion sync conditions
// that need an operator's attention. Only "conflict" exists in
// v1 — the run-branch → base merge couldn't reconcile content,
// so the run's output never reached the default branch. Kept as
// a typed kind so a future "diverged" / "push_rejected" variant
// slots in without a wire break.
type SyncStatusKind string

const (
	// SyncStatusConflict — the run-branch → base merge hit a
	// content conflict at run-completion time. The run is
	// (typically) `completed`, but its output is stranded on the
	// run branch; the default branch never got it. This is the
	// bug hunt's B-1: the documented parallel `branch: auto`
	// sweep makes every sibling after the first conflict on
	// shared output paths, and before this flag the only trace
	// was an ERROR in the per-run operator log.
	SyncStatusConflict SyncStatusKind = "conflict"
)

// SyncStatus is the structured form of the runs.sync_status JSON
// column. Surface readers (enju_run_status / enju runs /
// wire.Run consumers) unmarshal into this; the
// SetRunSyncStatus mutation's caller marshals from it. Field set
// varies by Kind — empty fields drop via omitempty.
//
// Distinct from BlockedBy on purpose: BlockedBy answers "why is
// this WAITING run stuck" and is cleared on every non-WAITING
// transition. SyncStatus answers "did this run's output reach
// the default branch" and MUST survive the terminal-completed
// state, because the data-loss outlives the run.
type SyncStatus struct {
	Kind          SyncStatusKind `json:"kind"`
	RunBranch     string         `json:"run_branch,omitempty"`
	BaseBranch    string         `json:"base_branch,omitempty"`
	ConflictFiles []string       `json:"conflict_files,omitempty"`
	// Hint is the copy-pasteable manual-resolution command, e.g.
	// "git checkout main && git merge load-test-sweep-2".
	Hint string `json:"hint,omitempty"`
	// Since is the RFC3339 timestamp the conflict was reported.
	Since string `json:"since,omitempty"`
}

// MarshalSyncStatus renders the SyncStatus as a compact JSON
// blob for the runs.sync_status column. A nil pointer marshals
// to the empty string (the "clear the flag" signal the
// SetRunSyncStatus mutation interprets).
func MarshalSyncStatus(s *SyncStatus) (string, error) {
	if s == nil {
		return "", nil
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	return string(b), nil
}

// ParseSyncStatus is the surface-side helper. Returns nil when
// the column is empty (sync clean / not attempted / mode:none)
// or the JSON is malformed (forward-compat: a future field
// shouldn't crash older readers, just render nothing). Exported
// so run_status / runs renderers and tests share one parse.
func ParseSyncStatus(raw string) *SyncStatus {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var s SyncStatus
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return nil
	}
	if s.Kind == "" {
		return nil
	}
	return &s
}
