package mcphandlers

import (
	"strings"
	"testing"
)

func TestRunBranchFromData(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"explicit-branch", `{"branch":"feature-x"}`, "feature-x"},
		{"missing-branch", `{"name":"run1"}`, ""},
		{"non-string-branch", `{"branch":42}`, ""},
		{"malformed-json", `not json`, ""},
		{"empty-bytes", ``, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := runBranchFromData([]byte(tc.in))
			if got != tc.want {
				t.Errorf("runBranchFromData(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestValidateDiagramPhase(t *testing.T) {
	good := []string{
		"initial",
		"final",
		"post_vote_stack_choice",
		"after_reject_v2",
		"phase.1",     // dot is fine
		"phase-1",
		"snake_case",
		strings.Repeat("a", 64), // exactly at the cap
	}
	for _, p := range good {
		if err := validateDiagramPhase(p); err != nil {
			t.Errorf("expected %q to pass, got %v", p, err)
		}
	}

	bad := []struct {
		phase     string
		wantMatch string
	}{
		{"", "required"},
		{strings.Repeat("a", 65), "too long"},
		{"bad/phase", "forbidden"},
		{"bad\\phase", "forbidden"},
		{"../etc/passwd", "forbidden"}, // both .. and /
		{"dots..dots", "forbidden"},
		{"null\x00byte", "forbidden"},
	}
	for _, tc := range bad {
		err := validateDiagramPhase(tc.phase)
		if err == nil {
			t.Errorf("expected %q to fail", tc.phase)
			continue
		}
		if !strings.Contains(err.Error(), tc.wantMatch) {
			t.Errorf("for %q expected error to mention %q, got %q", tc.phase, tc.wantMatch, err)
		}
	}
}
