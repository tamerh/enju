package mcphandlers

import (
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// TestFormatNotificationsMarkers pins the user-visible output
// shape: leading "* " for unread (seq > lastReadSeq), two spaces
// for read. No emoji, no fancy formatting.
func TestFormatNotificationsMarkers(t *testing.T) {
	now := time.Date(2026, 5, 1, 13, 42, 0, 0, time.UTC)
	matches := []service.Notification{
		{Seq: 5, Ts: now, RuleName: "issue_filed", Message: "Issue filed by @alice"},
		{Seq: 3, Ts: now.Add(-time.Hour), RuleName: "branch_merged", Message: "Topic merged for 1:1:t"},
		{Seq: 2, Ts: now.Add(-2 * time.Hour), RuleName: "run_completed", Message: "Run completed"},
	}
	// lastReadSeq = 3 → seq 5 is unread, seqs 3 and 2 are read.
	out := formatNotifications(matches, 3)

	lines := strings.Split(out, "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), out)
	}
	if !strings.HasPrefix(lines[0], "* ") {
		t.Errorf("seq 5 (unread) should start with '* ', got: %q", lines[0])
	}
	if !strings.HasPrefix(lines[1], "  ") || strings.HasPrefix(lines[1], "* ") {
		t.Errorf("seq 3 (read) should start with two spaces, not '* ', got: %q", lines[1])
	}
	if !strings.HasPrefix(lines[2], "  ") || strings.HasPrefix(lines[2], "* ") {
		t.Errorf("seq 2 (read) should start with two spaces, got: %q", lines[2])
	}
	if !strings.Contains(out, "Issue filed by @alice") {
		t.Errorf("expected message body in output, got: %q", out)
	}
}

// TestFormatNotificationsEmpty pins the no-events output.
func TestFormatNotificationsEmpty(t *testing.T) {
	out := formatNotifications(nil, 0)
	if out != "(no notifications)" {
		t.Errorf("empty list should render as '(no notifications)', got: %q", out)
	}
}
