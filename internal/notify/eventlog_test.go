package notify

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAppendEventToLogWritesJSONL pins the substrate contract:
// each event arrives as one JSON line in the project's
// enju/events/live.jsonl, including seq for cursor-based replay
// by Tier 2 consumers.
func TestAppendEventToLogWritesJSONL(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enju", "events", "live.jsonl")

	events := []Event{
		{Seq: 1, Timestamp: time.Date(2026, 5, 1, 12, 0, 0, 0, time.UTC), Type: "task_completed", TaskID: "1:1:draft"},
		{Seq: 2, Timestamp: time.Date(2026, 5, 1, 12, 0, 1, 0, time.UTC), Type: "branch_merged", TaskID: "1:1:draft"},
		{Seq: 3, Timestamp: time.Date(2026, 5, 1, 12, 0, 2, 0, time.UTC), Type: "issue_filed", Citizen: "alice"},
	}
	for _, ev := range events {
		if err := appendEventToLog(path, ev); err != nil {
			t.Fatalf("append seq=%d: %v", ev.Seq, err)
		}
	}

	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(body), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), body)
	}
	for i, line := range lines {
		var got Event
		if err := json.Unmarshal([]byte(line), &got); err != nil {
			t.Errorf("line %d: invalid JSON: %v", i, err)
			continue
		}
		if got.Seq != int64(i+1) {
			t.Errorf("line %d: seq = %d, want %d", i, got.Seq, i+1)
		}
		if got.Type != events[i].Type {
			t.Errorf("line %d: type = %q, want %q", i, got.Type, events[i].Type)
		}
	}
}

// TestAppendEventToLogSelfInstallsGitignore pins the gitignore
// drop: events dir gets a `.gitignore` of `*` on first write so
// the local stream never lands in git, regardless of whether the
// caller knows to ignore it. Self-installing per clone.
func TestAppendEventToLogSelfInstallsGitignore(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enju", "events", "live.jsonl")
	if err := appendEventToLog(path, Event{Seq: 1, Type: "x"}); err != nil {
		t.Fatal(err)
	}
	gi := filepath.Join(dir, "enju", "events", ".gitignore")
	body, err := os.ReadFile(gi)
	if err != nil {
		t.Fatalf("read gitignore: %v", err)
	}
	if string(body) != "*\n" {
		t.Errorf("gitignore body = %q, want %q", string(body), "*\n")
	}
}

// TestAppendEventToLogEmptyPathIsNoOp pins the test-friendly
// degradation: configs without a ProjectDir get no-op file ops
// (instead of writing to "/enju/events/live.jsonl" or similar
// surprising default).
func TestAppendEventToLogEmptyPathIsNoOp(t *testing.T) {
	if err := appendEventToLog("", Event{Seq: 1, Type: "x"}); err != nil {
		t.Errorf("empty path should be no-op, got: %v", err)
	}
}
