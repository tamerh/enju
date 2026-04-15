package mcpserver

import (
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
	out := formatVotingBlock("vote", 3, 3, "majority", "collecting", voteSubs, []string{"charlie"})

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
	out := formatVotingBlock("review", 3, 0, "", "accepted", voteSubs, nil)

	mustContain(t, out,
		"── Review ──",
		// Default quorum for a review with no explicit
		// min_quorum is citizens — should always render.
		"Citizens: 3 slots, quorum 3, threshold unanimous-approve",
		"unanimous-approve (any reject vetoes)",
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
	out := formatVotingBlock("review", 3, 0, "", "ready",
		[]interface{}{}, []string{"alice", "bob"})
	mustContain(t, out,
		"── Review ──",
		"accepting claims (2/3 claimed, 0/3 reviewed)",
		"Claimed:  alice, bob (not yet reviewed)",
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
