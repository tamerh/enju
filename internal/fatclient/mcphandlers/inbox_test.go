package mcphandlers

import (
	"strings"
	"testing"
)

// FormatInbox is a re-export of internal/inbox.FormatInbox.
// Detailed projection + rendering tests live in internal/inbox/;
// this file just pins that the re-export wires through correctly
// and keeps a couple of shape regressions guarded at the
// mcpserver layer where callers consume it.

// TestFormatInbox_Empty pins the no-items message — assistants
// pattern-match this to skip rendering.
func TestFormatInbox_Empty(t *testing.T) {
	got := FormatInbox(nil)
	if got != "(no tasks waiting on you)" {
		t.Errorf("empty inbox = %q, want '(no tasks waiting on you)'", got)
	}
}

// TestFormatInbox_BasicShape pins the header + per-task shape via
// the re-exported FormatInbox.
func TestFormatInbox_BasicShape(t *testing.T) {
	rows := []InboxRow{
		{
			TaskID: "5:1:review", Action: "review",
			Upstream: []InboxUpstreamRow{
				{TaskID: "5:1:abstract", Action: "answer", CommitSHA: "abc1234", Content: "The TP53 gene encodes...\nadditional line"},
			},
		},
	}
	out := FormatInbox(rows)
	mustContain := []string{
		"Inbox: 1 task(s) waiting on you",
		"[5:1:review] review",
		"Upstream [5:1:abstract] answer (commit abc1234)",
		"  The TP53 gene encodes...",
		"  additional line",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("output missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestFormatInbox_EmptyUpstreamContent pins the v1 limitation
// callout: compute/vote parents have no inlined content but
// still surface task_id + commit_sha. The hint tells the
// reader to pull from git.
func TestFormatInbox_EmptyUpstreamContent(t *testing.T) {
	rows := []InboxRow{
		{
			TaskID: "5:1:review", Action: "review",
			Upstream: []InboxUpstreamRow{
				{TaskID: "5:1:analyze", Action: "compute", CommitSHA: "def5678", Content: ""},
			},
		},
	}
	out := FormatInbox(rows)
	if !strings.Contains(out, "no inlined content") {
		t.Errorf("expected empty-content callout, got:\n%s", out)
	}
	if !strings.Contains(out, "def5678") {
		t.Errorf("expected commit_sha to still surface, got:\n%s", out)
	}
}

// TestFormatInbox_NoUpstream pins the "no parents" branch:
// inbox items without depends_on (direct-assigned answer/vote
// tasks) render cleanly without a missing-section error.
func TestFormatInbox_NoUpstream(t *testing.T) {
	rows := []InboxRow{
		{
			TaskID: "5:1:askyou", Action: "answer",
			Upstream: nil,
		},
	}
	out := FormatInbox(rows)
	if !strings.Contains(out, "(no upstream submissions)") {
		t.Errorf("expected no-upstream callout, got:\n%s", out)
	}
}
