package test

// Helpers for tests that need to seed a project's
// {projectDir}/.enju/events/live.jsonl directly (the
// inbox-projection tests, primarily). Defined locally so the
// test package doesn't pull internal/notify just for the JSON
// shape — we just need to write events the supervisor format
// expects, then assert downstream consumers (enju_inbox) read
// them correctly.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// notifyEvent mirrors the wire shape supervisor writes to
// live.jsonl, sufficient for inbox-projection tests.
type notifyEvent struct {
	Seq       int64     `json:"seq"`
	Timestamp time.Time `json:"ts"`
	Type      string    `json:"type"`
	Subtype   string    `json:"subtype,omitempty"`
	TaskID    string    `json:"task_id,omitempty"`
	Citizen   string    `json:"citizen,omitempty"`
	AssignTo  string    `json:"assign_to,omitempty"`
}

// seedLiveJSONL writes events into a project's live.jsonl. Used
// to set up the substrate without driving the full poll loop.
func seedLiveJSONL(t *testing.T, projectDir string, events []notifyEvent) {
	t.Helper()
	path := filepath.Join(projectDir, ".enju", "events", "live.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, ev := range events {
		line, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(line, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}
