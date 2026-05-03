package mcphandlers

import (
	"github.com/enju-ai/enju/internal/common/format"
	"strings"
	"testing"
)

// TestFormatVotingBlockVoteShape renders a typical multi-voter
// vote task mid-collection and asserts the vote-specific
// vocabulary (── Voting ── header, threshold label, Voters:
// line, "votes" suffix).
func TestFormatVotingBlockVoteShape(t *testing.T) {
	voteSubs := []interface{}{
		map[string]interface{}{"username": "alice", "option": "duckdb"},
		map[string]interface{}{"username": "bob", "option": "duckdb"},
	}
	out := format.VotingBlock("vote", 3, 3, "majority", "collecting", voteSubs, []string{"charlie"}, "", "", false, "", "")

	mustContain(t, out,
		"── Voting ──",
		"Citizens: 3 slots, quorum 3, threshold majority",
		"Status:   ⏳ collecting (2/3 votes)",
		"Tally:",
		"duckdb=2",
		"Voters:",
		"alice→duckdb",
		"bob→duckdb",
		"Claimed:  charlie (not yet voted)",
	)
	mustNotContain(t, out,
		"── Review ──",
		"reviews",
		"unanimous-approve",
		"Reviewers:",
		// Vocabulary unification: the formatter must say
		// "threshold", never "rule".
		"rule majority",
		"rule plurality",
	)
}

// TestFormatVotingBlockReviewShape renders a multi-reviewer
// review task and asserts the review-specific vocabulary:
// ── Review ── header, unanimous-approve label, Reviewers:
// line, "reviews" suffix, "(not yet reviewed)" on active
// claimants.
func TestFormatVotingBlockReviewShape(t *testing.T) {
	voteSubs := []interface{}{
		map[string]interface{}{"username": "alice", "option": "approve"},
		map[string]interface{}{"username": "bob", "option": "approve"},
		map[string]interface{}{"username": "charlie", "option": "approve"},
	}
	out := format.VotingBlock("review", 3, 0, "", "accepted", voteSubs, nil, "", "", false, "", "")

	mustContain(t, out,
		"── Review ──",
		// Default quorum for a review with no explicit
		// min_quorum is citizens — should always render.
		"Citizens: 3 slots, quorum 3, threshold any-reject-kills",
		"Status:   ✓ resolved (3/3 reviews)",
		"Tally:",
		"approve=3",
		"Reviewers:",
		"alice→approve",
		"bob→approve",
		"charlie→approve",
	)
	mustNotContain(t, out,
		"── Voting ──",
		"Voters:",
		"votes)",
		"majority",
		"plurality",
	)
}

// TestFormatVotingBlockReviewReadyPhase covers the "accepting
// claims" status line for review tasks — the phrasing should
// say "reviewed" instead of "submitted" so the text reads
// naturally.
func TestFormatVotingBlockReviewReadyPhase(t *testing.T) {
	out := format.VotingBlock("review", 3, 0, "", "ready",
		[]interface{}{}, []string{"alice", "bob"}, "", "", false, "", "")
	mustContain(t, out,
		"── Review ──",
		"accepting claims (2/3 claimed, 0/3 reviewed)",
		"Claimed:  alice, bob (not yet reviewed)",
	)
}

// TestFormatVotingBlockBlindFilter covers the blind-visibility
// filter: during COLLECTING, only the viewer's own submission
// is rendered, sibling ballots are hidden, and a trailing
// "N siblings hidden" note explains the blank slots.
func TestFormatVotingBlockBlindFilter(t *testing.T) {
	voteSubs := []interface{}{
		map[string]interface{}{"username": "alice", "option": "approve"},
		map[string]interface{}{"username": "bob", "option": "reject"},
		map[string]interface{}{"username": "charlie", "option": "approve"},
	}
	// Viewer is bob — should only see bob's ballot plus a hint.
	out := format.VotingBlock("review", 3, 0, "", "collecting", voteSubs, nil, "blind", "bob", false, "", "")
	mustContain(t, out,
		"bob→reject",
		"sibling ballots hidden",
	)
	mustNotContain(t, out,
		"alice→approve",
		"charlie→approve",
	)
}

// TestFormatVotingBlockBlindOpenOnceAccepted confirms that
// blind mode only applies while the task is COLLECTING.
// Once the task resolves to accepted, everyone sees everything.
func TestFormatVotingBlockBlindOpenOnceAccepted(t *testing.T) {
	voteSubs := []interface{}{
		map[string]interface{}{"username": "alice", "option": "approve"},
		map[string]interface{}{"username": "bob", "option": "reject"},
		map[string]interface{}{"username": "charlie", "option": "approve"},
	}
	out := format.VotingBlock("review", 3, 0, "", "accepted", voteSubs, nil, "blind", "bob", false, "", "")
	mustContain(t, out,
		"alice→approve",
		"bob→reject",
		"charlie→approve",
	)
	mustNotContain(t, out,
		"sibling ballots hidden",
	)
}

// TestWriteArtifactsDisplayHandlesObjectForm pins the formatter's
// ability to render writes_artifacts whether the wire carries the
// legacy bare-string form or the current {path,track} object form.
// Regression guard for a silent drop: the old extractor
// type-asserted each element as string and returned empty when the
// wire shape became polymorphic — the entire "Writes" block then
// vanished from claim / get_task output.
func TestWriteArtifactsDisplayHandlesObjectForm(t *testing.T) {
	// Object form — the current wire shape after Phase A.
	obj := []interface{}{
		map[string]interface{}{"path": "out/summary.json", "track": true},
		map[string]interface{}{"path": "out/big.bam", "track": false},
	}
	got := format.WriteArtifactPathsFromAny(obj)
	if len(got) != 2 || got[0] != "out/summary.json" || got[1] != "out/big.bam" {
		t.Errorf("object form wrong: %v", got)
	}

	// Legacy bare-string form — what pre-Phase-A DB rows
	// surface. Must still work for any consumer who hasn't
	// re-saved.
	legacy := []interface{}{"out/a.md", "out/b.md"}
	got = format.WriteArtifactPathsFromAny(legacy)
	if len(got) != 2 || got[0] != "out/a.md" || got[1] != "out/b.md" {
		t.Errorf("legacy form wrong: %v", got)
	}

	// Mixed form (legacy + object in the same list, shouldn't
	// happen but must not panic).
	mixed := []interface{}{
		"bare/a.md",
		map[string]interface{}{"path": "obj/b.md", "track": true},
	}
	got = format.WriteArtifactPathsFromAny(mixed)
	if len(got) != 2 || got[0] != "bare/a.md" || got[1] != "obj/b.md" {
		t.Errorf("mixed form wrong: %v", got)
	}

	// Nil / empty / wrong-type inputs → nil, never panic.
	if format.WriteArtifactPathsFromAny(nil) != nil {
		t.Error("nil should return nil")
	}
	if got := format.WriteArtifactPathsFromAny([]interface{}{}); len(got) != 0 {
		t.Errorf("empty should return empty, got %v", got)
	}
}

// TestFormatGetTaskIncludesWritesArtifactsSection is the end-to-end
// regression guard: a task payload carrying the object-form
// writes_artifacts must render a visible "Writes" block in
// format.GetTask's output. Before the fix, format.StringSliceFromAny's
// string-only type assert silently dropped every entry and the
// entire Artifacts block disappeared.
func TestFormatGetTaskIncludesWritesArtifactsSection(t *testing.T) {
	task := map[string]interface{}{
		"id":          "1:1:compute",
		"state":       "ready",
		"action":      "compute",
		"task_def_id": "analyze",
		"writes_artifacts": []interface{}{
			map[string]interface{}{"path": "out/report.md", "track": true},
			map[string]interface{}{"path": "out/scratch.bam", "track": false},
		},
	}
	out := format.ArtifactsSchema(
		format.WriteArtifactPathsFromAny(task["reads_artifacts"]),
		format.WriteArtifactPathsFromAny(task["writes_artifacts"]),
	)
	mustContain(t, out,
		"── Artifacts",
		"Writes",
		"out/report.md",
		"out/scratch.bam",
	)
}

func mustContain(t *testing.T, s string, wants ...string) {
	t.Helper()
	for _, w := range wants {
		if !strings.Contains(s, w) {
			t.Errorf("expected output to contain %q, got:\n%s", w, s)
		}
	}
}

func mustNotContain(t *testing.T, s string, avoid ...string) {
	t.Helper()
	for _, a := range avoid {
		if strings.Contains(s, a) {
			t.Errorf("expected output to NOT contain %q, got:\n%s", a, s)
		}
	}
}
