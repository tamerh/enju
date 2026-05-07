package enjugit

import (
	"reflect"
	"testing"
)

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
	got := RenderEnjuTrailers(EnjuTrailers{TaskID: "2:1:draft"})
	if got != "Enju-Task-Complete: 2:1:draft" {
		t.Errorf("expected single-line task trailer, got: %q", got)
	}
}

func TestRenderEnjuTrailersEmptyTask(t *testing.T) {
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

func TestEnjuTrailersUntrackedRoundTrip(t *testing.T) {
	input := EnjuTrailers{
		TaskID:             "3:1:align",
		ExitCode:           0,
		ExitSet:            true,
		DurationSeconds:    42,
		Artifacts:          []string{"out/stats.json"},
		UntrackedArtifacts: []string{"reads/S1_R1.fq", "reads/S1_R2.fq"},
	}
	msg := "Task 3:1:align by @alice: result + 3 artifact(s)\n\n" +
		RenderEnjuTrailers(input)
	got := ParseEnjuTrailers(msg)
	if !reflect.DeepEqual(got, input) {
		t.Errorf("round trip lost fidelity:\ngot:  %+v\nwant: %+v", got, input)
	}
}

func TestEnjuTrailersLegacyWithoutUntrackedDecodes(t *testing.T) {
	msg := "Task 3:1:t by @alice: result\n\n" +
		"Enju-Task-Complete: 3:1:t\n" +
		"Enju-Exit: 0\n" +
		"Enju-Artifacts: out/stats.json"
	got := ParseEnjuTrailers(msg)
	if got.TaskID != "3:1:t" || len(got.Artifacts) != 1 {
		t.Fatalf("legacy trailer decode failed: %+v", got)
	}
	if got.UntrackedArtifacts != nil {
		t.Errorf("expected nil UntrackedArtifacts, got %+v", got.UntrackedArtifacts)
	}
}

func TestParseEnjuTrailersIgnoresNonTrailerParagraphs(t *testing.T) {
	msg := "Task 3:1:foo by @alice: result\n\n" +
		"Enju-Task-Complete: 99:99:bogus (this is in the body)\n" +
		"Some more body text.\n\n" +
		"Enju-Task-Complete: 3:1:foo\n" +
		"Enju-Exit: 0"
	got := ParseEnjuTrailers(msg)
	if got.TaskID != "3:1:foo" {
		t.Errorf("expected TaskID from trailer paragraph, got %q", got.TaskID)
	}
}

func TestParseEnjuTrailersToleratesMalformedNumbers(t *testing.T) {
	msg := "Subject\n\nEnju-Task-Complete: 1:1:t\nEnju-Exit: oops"
	got := ParseEnjuTrailers(msg)
	if got.TaskID != "1:1:t" {
		t.Errorf("expected TaskID preserved despite bad Exit, got: %+v", got)
	}
	if got.ExitSet {
		t.Errorf("ExitSet should be false for unparseable Exit")
	}
}

func TestParseEnjuTrailersEmptyMessage(t *testing.T) {
	got := ParseEnjuTrailers("")
	if got.TaskID != "" || got.ExitSet || len(got.Artifacts) != 0 {
		t.Errorf("expected zero trailers for empty message, got: %+v", got)
	}
}

func TestEnjuTrailersVerdictAndIterSeq(t *testing.T) {
	input := EnjuTrailers{
		TaskID:  "5:2:review",
		Verdict: "approve",
		IterSeq: 3,
	}
	got := ParseEnjuTrailers("subj\n\n" + RenderEnjuTrailers(input))
	if got.Verdict != "approve" || got.IterSeq != 3 {
		t.Errorf("verdict/iter trailers lost: %+v", got)
	}
}
