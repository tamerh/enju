package mcpgit

import (
	"reflect"
	"strings"
	"testing"
)

// Trailer emit + parse is a round-trip protocol between
// wrapper-side commits and reader-side scanners; if one drifts,
// state reconciliation silently breaks. These tests pin the
// emitted format, the parse tolerance for extra trailers, and
// the empty-state handling.

func TestRenderEnjuTrailersOrder(t *testing.T) {
	got := RenderEnjuTrailers(EnjuTrailers{
		TaskID:          "3:1:foo",
		ExitCode:        0,
		ExitSet:         true,
		DurationSeconds: 412,
		Artifacts:       []string{"sections/TP53/7_pathways.md"},
	})
	want := "Enju-Task-Complete: 3:1:foo\n" +
		"Enju-Exit: 0\n" +
		"Enju-Duration-Seconds: 412\n" +
		"Enju-Artifacts: sections/TP53/7_pathways.md"
	if got != want {
		t.Errorf("render: mismatch.\ngot:\n%s\n\nwant:\n%s", got, want)
	}
}

func TestRenderEnjuTrailersOmitsUnsetFields(t *testing.T) {
	// Answer/review commits carry only TaskID. No Exit trailer,
	// no Duration trailer, no Artifacts trailer when the lists
	// are empty and numeric fields unset.
	got := RenderEnjuTrailers(EnjuTrailers{TaskID: "2:1:draft"})
	if got != "Enju-Task-Complete: 2:1:draft" {
		t.Errorf("expected single-line task trailer, got: %q", got)
	}
}

func TestRenderEnjuTrailersEmptyTask(t *testing.T) {
	// No task id ⇒ no trailer block. Guards against a commit
	// message accidentally getting a blank Enju-Task-Complete
	// line that a scanner would pick up as "task id = empty".
	got := RenderEnjuTrailers(EnjuTrailers{})
	if got != "" {
		t.Errorf("expected empty render for empty trailers, got: %q", got)
	}
}

func TestParseEnjuTrailersRoundTrip(t *testing.T) {
	input := EnjuTrailers{
		TaskID:          "3:1:foo:section_7",
		ExitCode:        0,
		ExitSet:         true,
		DurationSeconds: 412,
		Artifacts:       []string{"a.md", "b.md"},
	}
	// Simulate a full commit message: subject + body + trailers.
	msg := "Task 3:1:foo:section_7 by @alice: result + 2 artifact(s)\n\n" +
		"Artifacts: a.md, b.md\n\n" +
		"Co-Authored-By: Claude (claude-opus-4) <noreply@anthropic.com>\n" +
		"AI-Model: claude-opus-4\n" +
		RenderEnjuTrailers(input)
	got := ParseEnjuTrailers(msg)
	if !reflect.DeepEqual(got, input) {
		t.Errorf("round trip mismatch.\ngot:  %+v\nwant: %+v", got, input)
	}
}

func TestParseEnjuTrailersIgnoresNonTrailerParagraphs(t *testing.T) {
	// A commit message whose BODY contains text that looks
	// trailer-shaped (`Key: val`) must not be picked up as a
	// trailer. Only the final paragraph is the trailer zone.
	msg := "Task 3:1:foo by @alice: result\n\n" +
		"Enju-Task-Complete: 99:99:bogus (this is in the body)\n" +
		"Some more body text.\n\n" +
		"Enju-Task-Complete: 3:1:foo\n" +
		"Enju-Exit: 0"
	got := ParseEnjuTrailers(msg)
	if got.TaskID != "3:1:foo" {
		t.Errorf("expected TaskID from trailer paragraph, got %q (full: %+v)", got.TaskID, got)
	}
}

func TestParseEnjuTrailersToleratesMalformedNumbers(t *testing.T) {
	// A wrapper bug that emits `Enju-Exit: oops` must not make
	// the scanner drop the whole commit — we still get the
	// task id and treat exit as unknown (zero-value, ExitSet
	// stays false).
	msg := "Subject\n\nEnju-Task-Complete: 1:1:t\nEnju-Exit: oops"
	got := ParseEnjuTrailers(msg)
	if got.TaskID != "1:1:t" {
		t.Errorf("expected TaskID preserved despite bad Exit, got: %+v", got)
	}
	if got.ExitSet {
		t.Errorf("ExitSet should be false for unparseable Exit, got: %+v", got)
	}
}

func TestParseEnjuTrailersEmptyMessage(t *testing.T) {
	got := ParseEnjuTrailers("")
	if got.TaskID != "" || got.ExitSet || len(got.Artifacts) != 0 {
		t.Errorf("expected zero trailers for empty message, got: %+v", got)
	}
}

func TestBuildCommitMessageEmitsTrailers(t *testing.T) {
	// Anchor the wrapper's actual output format: the commit
	// message the fetch-path scanner will see after a compute
	// task completes. Checks two things: (a) trailer paragraph
	// comes LAST, (b) Enju-Task-Complete trailer is present
	// alongside Co-Authored-By/AI-Model.
	msg := buildCommitMessage("3:1:foo", "alice", nil, "claude-opus-4", EnjuTrailers{
		TaskID:   "3:1:foo",
		ExitCode: 0,
		ExitSet:  true,
	})
	// Trailer paragraph must be parseable back out of the full
	// message — the protocol contract.
	got := ParseEnjuTrailers(msg)
	if got.TaskID != "3:1:foo" {
		t.Errorf("TaskID not recovered from built message: %+v\nmsg:\n%s", got, msg)
	}
	if !got.ExitSet || got.ExitCode != 0 {
		t.Errorf("Exit not recovered: %+v", got)
	}
	// Spot-check that both AI-Model and Enju-Task-Complete
	// share the same trailer paragraph (single blank-line
	// separator at the end). Two trailer paragraphs would
	// confuse `git interpret-trailers`.
	if strings.Count(msg, "\n\n") > 1 {
		t.Errorf("expected a single trailer paragraph, got message with multiple:\n%s", msg)
	}
}
