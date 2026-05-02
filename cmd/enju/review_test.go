package main

import (
	"strings"
	"testing"
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

// TestBuildReviewTemplate_BareTemplate pins the editor pre-fill:
// just task header + usage hint, no rich context. Reviewers run
// `enju inbox` first if they want to see the prompt + upstream
// submissions before writing.
func TestBuildReviewTemplate_BareTemplate(t *testing.T) {
	out := buildReviewTemplate("5:1:review")
	mustContain := []string{
		"Reviewing 5:1:review",
		"enju inbox <project_id>",
		"Lines starting with '#' are",
		"-decision",
	}
	for _, want := range mustContain {
		if !strings.Contains(out, want) {
			t.Errorf("template missing %q\nfull:\n%s", want, out)
		}
	}
	mustNotContain := []string{
		"Upstream",
		"This task's prompt:",
	}
	for _, banned := range mustNotContain {
		if strings.Contains(out, banned) {
			t.Errorf("bare template should not contain %q\nfull:\n%s", banned, out)
		}
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
