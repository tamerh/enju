package test

// End-to-end integration test for the enju_notifications MCP
// tool. Pins the read/unread transition: items are unread (`* `)
// on first call, read (`  `) on second call. mark_read=false
// leaves the cursor unchanged so items stay marked unread across
// calls.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// notifyEvent mirrors the JSON shape of one line in live.jsonl.
// Defined locally to avoid pulling internal/notify into the test
// package — we just need to write JSON the supervisor format
// expects.
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
	path := filepath.Join(projectDir, "enju", "events", "live.jsonl")
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

// TestNotificationsReadUnreadTransition pins the user-visible
// flow: first call shows fresh items as `*` (unread), second
// call shows them as `  ` (read). The mark-read cursor advances
// only when mark_read=true (the default).
func TestNotificationsReadUnreadTransition(t *testing.T) {
	h := newMCPHarness(t, "NotifyTester")
	projectID := h.createTestProject()

	// Force the workspace to materialize the project's clone dir
	// so live.jsonl has a place to live. createTestProject sets
	// up the bare remote; ForProject populates the local clone.
	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "notify-test"); err != nil {
		t.Fatalf("workspace.ForProject: %v", err)
	}
	projectDir := h.workspace.ProjectDir(projectID)
	if projectDir == "" {
		t.Fatal("project dir not resolvable after ForProject")
	}

	now := time.Now().UTC()
	seedLiveJSONL(t, projectDir, []notifyEvent{
		{Seq: 1, Timestamp: now.Add(-3 * time.Minute), Type: "branch_merged", TaskID: "1:1:t"},
		{Seq: 2, Timestamp: now.Add(-2 * time.Minute), Type: "issue_filed", Citizen: "alice"},
		{Seq: 3, Timestamp: now.Add(-1 * time.Minute), Type: "task_completed", TaskID: "1:1:t", Citizen: h.username},
	})

	// First call: all 3 should be unread (lead with "* ").
	res := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)
	unreadCount := strings.Count(out, "* ")
	if unreadCount != 3 {
		t.Errorf("first call: expected 3 unread markers, got %d. output:\n%s", unreadCount, out)
	}

	// Second call (default mark_read=true had advanced cursor):
	// all 3 should now be read.
	res2 := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
	})
	out2 := mcpText(res2)
	if strings.Contains(out2, "* ") {
		t.Errorf("second call: items should be marked read, but found '* ' in output:\n%s", out2)
	}
}

// TestNotificationsMarkReadFalsePeeks pins the peek behavior:
// mark_read=false returns the same view but doesn't advance the
// read cursor, so subsequent calls still show items as unread.
func TestNotificationsMarkReadFalsePeeks(t *testing.T) {
	h := newMCPHarness(t, "PeekTester")
	projectID := h.createTestProject()
	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "peek-test"); err != nil {
		t.Fatal(err)
	}
	projectDir := h.workspace.ProjectDir(projectID)
	now := time.Now().UTC()
	seedLiveJSONL(t, projectDir, []notifyEvent{
		{Seq: 1, Timestamp: now, Type: "issue_filed", Citizen: "alice"},
	})

	// Peek (don't mark read).
	res1 := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
		"mark_read":  false,
	})
	if !strings.Contains(mcpText(res1), "* ") {
		t.Errorf("peek call should show unread marker; got:\n%s", mcpText(res1))
	}

	// Peek again (still don't mark read). Should still be unread
	// because the previous call didn't advance the cursor.
	res2 := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
		"mark_read":  false,
	})
	if !strings.Contains(mcpText(res2), "* ") {
		t.Errorf("second peek should still show unread; got:\n%s", mcpText(res2))
	}

	// Now do a real read (default mark_read=true).
	h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
	})

	// Subsequent peek should now show as read.
	res3 := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
		"mark_read":  false,
	})
	if strings.Contains(mcpText(res3), "* ") {
		t.Errorf("after default-mark_read call, peek should show read; got:\n%s", mcpText(res3))
	}
}

// TestNotificationsDisableDefaults pins the opt-out: events of a
// type listed in disable_defaults don't appear in the output.
func TestNotificationsDisableDefaults(t *testing.T) {
	h := newMCPHarness(t, "OptOutTester")
	projectID := h.createTestProject()
	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "optout-test"); err != nil {
		t.Fatal(err)
	}
	projectDir := h.workspace.ProjectDir(projectID)

	// Drop a notify.yaml that mutes branch_merged.
	yamlPath := filepath.Join(projectDir, "enju", "notify.yaml")
	if err := os.MkdirAll(filepath.Dir(yamlPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(yamlPath, []byte("disable_defaults: [branch_merged]\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	now := time.Now().UTC()
	seedLiveJSONL(t, projectDir, []notifyEvent{
		{Seq: 1, Timestamp: now, Type: "branch_merged", TaskID: "1:1:t"},
		{Seq: 2, Timestamp: now, Type: "issue_filed", Citizen: "alice"},
	})

	res := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
	})
	out := mcpText(res)
	if strings.Contains(out, "Topic merged") {
		t.Errorf("branch_merged should be muted via disable_defaults, but found in output:\n%s", out)
	}
	if !strings.Contains(out, "Issue filed by @alice") {
		t.Errorf("issue_filed should still appear; got:\n%s", out)
	}
}

// TestNotificationsLimitClamps pins the safety on the limit
// parameter: caller asks for 100 items, only 3 match → 3 come
// back; caller asks for >100 → silently clamped at 100; <=0 →
// default of 20.
func TestNotificationsLimitClamps(t *testing.T) {
	h := newMCPHarness(t, "LimitTester")
	projectID := h.createTestProject()
	if _, err := h.workspace.ForProject(projectID, h.remoteFor(projectID), "limit-test"); err != nil {
		t.Fatal(err)
	}
	projectDir := h.workspace.ProjectDir(projectID)

	// Seed 30 matching events.
	now := time.Now().UTC()
	events := make([]notifyEvent, 30)
	for i := range events {
		events[i] = notifyEvent{
			Seq:       int64(i + 1),
			Timestamp: now.Add(time.Duration(i) * time.Second),
			Type:      "branch_merged",
			TaskID:    fmt.Sprintf("1:1:t%d", i+1),
		}
	}
	seedLiveJSONL(t, projectDir, events)

	// limit=5 → 5 lines.
	res := h.callOK(t, "enju_notifications", map[string]any{
		"project_id": float64(projectID),
		"limit":      float64(5),
	})
	lines := strings.Split(strings.TrimSpace(mcpText(res)), "\n")
	if len(lines) != 5 {
		t.Errorf("limit=5: expected 5 lines, got %d. output:\n%s", len(lines), mcpText(res))
	}
}
