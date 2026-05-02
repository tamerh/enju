package main

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/mcpserver"
)

// TestStripCommentLines pins git-style comment stripping in the
// editor flow. Lines starting with `#` (after optional leading
// whitespace) drop; everything else passes through. Outer
// whitespace gets trimmed.
func TestStripCommentLines(t *testing.T) {
	cases := []struct {
		name, in, want string
	}{
		{
			name: "drops_comment_only",
			in:   "# comment\n# another\n",
			want: "",
		},
		{
			name: "keeps_body",
			in:   "# header comment\nThe abstract is solid.\nNo concerns.\n# trailer\n",
			want: "The abstract is solid.\nNo concerns.",
		},
		{
			name: "indented_comment_dropped",
			in:   "  # leading whitespace\nbody line\n",
			want: "body line",
		},
		{
			name: "hash_in_middle_kept",
			in:   "this is # not a comment\n",
			want: "this is # not a comment",
		},
		{
			name: "trims_surrounding_blank_lines",
			in:   "\n\nreal content\n\n",
			want: "real content",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := stripCommentLines(c.in)
			if got != c.want {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}
}

// TestBuildReviewTemplate_BareFallback pins the no-context
// fallback: when the inbox fetch fails or returns no match,
// the template still renders, with just the task id and the
// usage instructions. The user gets a usable editor, not an
// error.
func TestBuildReviewTemplate_BareFallback(t *testing.T) {
	out := buildReviewTemplate("5:1:review", nil)
	if !strings.Contains(out, "Reviewing 5:1:review") {
		t.Errorf("missing task header, got:\n%s", out)
	}
	if !strings.Contains(out, "Lines starting with '#' are") {
		t.Errorf("missing usage instructions, got:\n%s", out)
	}
	if strings.Contains(out, "Upstream") {
		t.Errorf("bare fallback should not have Upstream section, got:\n%s", out)
	}
}

// TestBuildReviewTemplate_RichContext pins the populated
// editor template: action, prompt, and upstream submissions
// all surface as comment lines so the reviewer can read what
// they're reviewing inline.
func TestBuildReviewTemplate_RichContext(t *testing.T) {
	ctx := &mcpserver.InboxRow{
		TaskID: "5:1:review",
		Action: "review",
		Prompt: "review the abstract for clarity",
		Upstream: []mcpserver.InboxUpstreamRow{
			{TaskID: "5:1:abstract", Action: "answer", CommitSHA: "abc1234", Content: "The TP53 gene encodes a tumor\nsuppressor protein."},
		},
	}
	out := buildReviewTemplate("5:1:review", ctx)

	mustContain := []string{
		"Reviewing 5:1:review — review",
		"This task's prompt:",
		"# > review the abstract for clarity",
		"Upstream 5:1:abstract (answer) commit abc1234:",
		"# > The TP53 gene encodes a tumor",
		"# > suppressor protein.",
		"Decision: pass -decision flag",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("template missing %q\nfull:\n%s", want, out)
		}
	}
}

// TestBuildReviewTemplate_TruncatedPromptHint pins the trailer
// note when the inbox-side cap clipped the task prompt — the
// reviewer should know to fetch the full text if the snippet
// is incomplete.
func TestBuildReviewTemplate_TruncatedPromptHint(t *testing.T) {
	ctx := &mcpserver.InboxRow{
		TaskID:          "5:1:review",
		Action:          "review",
		Prompt:          "trailing fragment",
		PromptTruncated: true,
	}
	out := buildReviewTemplate("5:1:review", ctx)
	if !strings.Contains(out, "[truncated") {
		t.Errorf("expected truncation marker, got:\n%s", out)
	}
}

// TestBuildReviewTemplate_EmptyUpstreamContent pins the
// compute/vote-parent callout: still surface task_id +
// commit_sha, suggest pulling from git.
func TestBuildReviewTemplate_EmptyUpstreamContent(t *testing.T) {
	ctx := &mcpserver.InboxRow{
		TaskID: "5:1:review",
		Action: "review",
		Prompt: "review compute output",
		Upstream: []mcpserver.InboxUpstreamRow{
			{TaskID: "5:1:analyze", Action: "compute", CommitSHA: "def5678"},
		},
	}
	out := buildReviewTemplate("5:1:review", ctx)
	if !strings.Contains(out, "Upstream 5:1:analyze (compute) commit def5678") {
		t.Errorf("expected upstream header even without content, got:\n%s", out)
	}
	if !strings.Contains(out, "no inlined content") {
		t.Errorf("expected pull-from-git hint, got:\n%s", out)
	}
}

// TestIsValidDecision pins the decision-verb whitelist. Any
// drift from internal/mcpserver/submit.go's
// validateReviewDecision is a regression — both surfaces must
// accept the same set verbatim.
func TestIsValidDecision(t *testing.T) {
	for _, v := range []string{"approve", "request_changes", "reject", "comment"} {
		if !isValidDecision(v) {
			t.Errorf("expected %q valid", v)
		}
	}
	for _, v := range []string{"", "accept", "yes", "no", "request-changes", "APPROVE"} {
		if isValidDecision(v) {
			t.Errorf("expected %q invalid", v)
		}
	}
}
