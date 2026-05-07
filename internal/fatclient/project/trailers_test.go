package project

import (
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
)

// TestBuildCommitMessageEmitsTrailers anchors project.buildCommitMessage's
// output format: the commit message the fetch-path scanner sees after a
// compute task completes. Checks two things: (a) the trailer paragraph
// comes LAST, (b) the Enju-Task-Complete trailer shares the paragraph
// with Co-Authored-By/AI-Model. The trailer parse + render contract
// itself is covered in enjugit/parsed_trailers_test.go — this test only
// exists to pin project's commit-message *composition*.
func TestBuildCommitMessageEmitsTrailers(t *testing.T) {
	msg := buildCommitMessage("3:1:foo", "alice", nil, "claude-opus-4", enjugit.EnjuTrailers{
		TaskID:   "3:1:foo",
		ExitCode: 0,
		ExitSet:  true,
	})
	// Trailer paragraph must be parseable back out of the full message
	// — the protocol contract.
	got := enjugit.ParseEnjuTrailers(msg)
	if got.TaskID != "3:1:foo" {
		t.Errorf("TaskID not recovered from built message: %+v\nmsg:\n%s", got, msg)
	}
	if !got.ExitSet || got.ExitCode != 0 {
		t.Errorf("Exit not recovered: %+v", got)
	}
	// Spot-check that both AI-Model and Enju-Task-Complete share the
	// same trailer paragraph (single blank-line separator at the end).
	// Two trailer paragraphs would confuse `git interpret-trailers`.
	if strings.Count(msg, "\n\n") > 1 {
		t.Errorf("expected a single trailer paragraph, got message with multiple:\n%s", msg)
	}
}
