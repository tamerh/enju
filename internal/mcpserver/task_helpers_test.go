package mcpserver

import (
	"strings"
	"testing"
)

func TestStringSliceNonNil(t *testing.T) {
	if got := stringSliceNonNil(nil); got == nil {
		t.Fatal("nil input should become empty slice")
	} else if len(got) != 0 {
		t.Fatalf("expected empty, got %v", got)
	}

	in := []string{"a", "b"}
	got := stringSliceNonNil(in)
	if len(got) != 2 || got[0] != "a" || got[1] != "b" {
		t.Fatalf("non-nil input should round-trip, got %v", got)
	}
}

func TestEncodeParamEnv(t *testing.T) {
	cases := []struct {
		name string
		in   interface{}
		want string
	}{
		{"nil", nil, ""},
		{"string", "hello", "hello"},
		{"bool-true", true, "true"},
		{"bool-false", false, "false"},
		{"int-via-float64", float64(42), "42"},
		{"negative-int", float64(-7), "-7"},
		{"real-float", 3.14, "3.14"},
		{"list-of-strings", []interface{}{"a", "b", "c"}, "a,b,c"},
		{"list-of-ints", []interface{}{float64(1), float64(2)}, "1,2"},
		{"empty-list", []interface{}{}, ""},
		{"nested-list", []interface{}{"a", []interface{}{"x", "y"}}, "a,x,y"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := encodeParamEnv(tc.in)
			if got != tc.want {
				t.Errorf("encodeParamEnv(%v) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// TestEncodeParamEnvMapFallback covers the JSON fallback branch for
// unexpected types (nested structs / maps). The exact rendering is
// not load-bearing — we just need a non-empty representation so
// scripts see something rather than Go's default "map[...]".
func TestEncodeParamEnvMapFallback(t *testing.T) {
	got := encodeParamEnv(map[string]interface{}{"k": "v"})
	if got == "" {
		t.Fatal("expected non-empty fallback representation")
	}
	if !strings.Contains(got, "v") {
		t.Errorf("expected value in rendering, got %q", got)
	}
}

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
