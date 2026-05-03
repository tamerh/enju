package mcpserver

import (
	"strings"
	"testing"
)

func TestParseReviewsTarget(t *testing.T) {
	cases := []struct {
		in              string
		wantDefID       string
		wantInstanceKey string
	}{
		// Singleton review: just the def id.
		{"draft", "draft", ""},
		// Per-instance review: "instanceKey:defID".
		{"alpha:expand", "expand", "alpha"},
		{"iter-3:summarize", "summarize", "iter-3"},
		// Empty passes through.
		{"", "", ""},
		// Pathological leading-colon shape: NOT split. The
		// materializer never writes ":foo" (instance keys
		// aren't empty when a colon is present), so we treat
		// the whole string as the def id rather than producing
		// (defID="foo", instanceKey="") — matches the engine
		// side's parseReviewsTargetForMerge behavior so the
		// merge collector and feedback resolver agree.
		{":foo", ":foo", ""},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			gotDef, gotInst := parseReviewsTarget(tc.in)
			if gotDef != tc.wantDefID || gotInst != tc.wantInstanceKey {
				t.Fatalf("parseReviewsTarget(%q) = (%q, %q), want (%q, %q)",
					tc.in, gotDef, gotInst, tc.wantDefID, tc.wantInstanceKey)
			}
		})
	}
}

// TestValidateReviewDecisionPhrasingIsShared covers the contract that
// validateReviewDecision defers to invalidDecisionMessage for unknown
// inputs (a previous bug had three different wordings in three places).
// The broader happy/sad path table lives in TestValidateReviewDecision
// in server_test.go.
func TestValidateReviewDecisionPhrasingIsShared(t *testing.T) {
	got := validateReviewDecision("maybe")
	want := invalidDecisionMessage("maybe")
	if got != want {
		t.Errorf("unknown decision phrasing drifted:\n got %q\nwant %q", got, want)
	}
}

func TestInvalidDecisionMessage(t *testing.T) {
	msg := invalidDecisionMessage("bogus")
	for _, want := range []string{`"bogus"`, "approve", "reject", "request_changes", "comment"} {
		if !strings.Contains(msg, want) {
			t.Errorf("invalidDecisionMessage missing %q: %q", want, msg)
		}
	}
}

func TestValidateVoteOption(t *testing.T) {
	opts := `[{"id":"duckdb"},{"id":"postgres"},{"id":"sqlite"}]`

	// Valid option: empty error.
	if msg := validateVoteOption("duckdb", opts); msg != "" {
		t.Errorf("valid option should pass, got %q", msg)
	}

	// Missing option: lists the choices.
	missing := validateVoteOption("", opts)
	for _, want := range []string{"required", "duckdb", "postgres", "sqlite"} {
		if !strings.Contains(missing, want) {
			t.Errorf("missing-option message lacks %q: %q", want, missing)
		}
	}

	// Unknown option: must quote the bad value and list valid ones.
	bad := validateVoteOption("mysql", opts)
	for _, want := range []string{`"mysql"`, "duckdb", "postgres", "sqlite"} {
		if !strings.Contains(bad, want) {
			t.Errorf("unknown-option message lacks %q: %q", want, bad)
		}
	}

	// Empty optionsJSON falls through (defer to coordinator).
	if msg := validateVoteOption("anything", ""); msg != "" {
		t.Errorf("empty optionsJSON should defer, got %q", msg)
	}

	// Malformed optionsJSON also falls through.
	if msg := validateVoteOption("anything", "not json"); msg != "" {
		t.Errorf("malformed optionsJSON should defer, got %q", msg)
	}

	// Empty options list also defers.
	if msg := validateVoteOption("x", "[]"); msg != "" {
		t.Errorf("empty options list should defer, got %q", msg)
	}
}

func TestDecorateCoordinatorRejection(t *testing.T) {
	// Plain rejection: wrapped but no sync hint.
	plain := decorateCoordinatorRejection("something else went wrong")
	if !strings.HasPrefix(plain, "coordinator rejected report:") {
		t.Errorf("missing prefix: %q", plain)
	}
	if strings.Contains(plain, "enju_project_sync") {
		t.Errorf("plain rejection shouldn't get sync hint: %q", plain)
	}

	// Stale-state rejection: hint appears.
	staleMessages := []string{
		"commit SHA mismatch, your report is stale",
		"Unknown commit abc123",
		"commit not found in repo",
		"invalid state transition from collecting to accepted",
		"task is not in state claimed",
		"run already accepted",
		"superseded by newer submission",
	}
	for _, m := range staleMessages {
		got := decorateCoordinatorRejection(m)
		if !strings.Contains(got, "enju_project_sync") {
			t.Errorf("expected sync hint for %q, got %q", m, got)
		}
	}

	// Case-insensitive match.
	shouting := decorateCoordinatorRejection("UNKNOWN COMMIT deadbeef")
	if !strings.Contains(shouting, "enju_project_sync") {
		t.Errorf("case-insensitive match failed: %q", shouting)
	}
}
