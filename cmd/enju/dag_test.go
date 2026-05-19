package main

import (
	"reflect"
	"testing"
)

// TestHoistFlagsBeforePositionals is the bug hunt B-6 regression:
// `enju dag <seq> --format json` (the command's own documented
// positional-first syntax) must reach the flag parser with the
// flag intact. Go's flag package stops at the first non-flag arg,
// so without the hoist `--format json` is silently dropped and
// the command exits 2 on its own usage string.
func TestHoistFlagsBeforePositionals(t *testing.T) {
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{
			"documented broken form: positional then value flag",
			[]string{"15", "--format", "json"},
			[]string{"--format", "json", "15"},
		},
		{
			"--format=json after positional",
			[]string{"15", "--format=json"},
			[]string{"--format=json", "15"},
		},
		{
			"already-correct form is unchanged",
			[]string{"--format=json", "15"},
			[]string{"--format=json", "15"},
		},
		{
			"flag-before-positional bare value form unchanged",
			[]string{"--format", "json", "15"},
			[]string{"--format", "json", "15"},
		},
		{
			"multiple flags interleaved with the positional",
			[]string{"15", "--project", "12", "--format", "mermaid"},
			[]string{"--project", "12", "--format", "mermaid", "15"},
		},
		{
			"single-dash long flag spelling",
			[]string{"15", "-format", "json"},
			[]string{"-format", "json", "15"},
		},
		{
			"plain positional only",
			[]string{"15"},
			[]string{"15"},
		},
		{
			"-- ends flag processing; trailing token stays positional",
			[]string{"--format", "json", "--", "15"},
			[]string{"--format", "json", "15"},
		},
		{
			"no args",
			nil,
			nil,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := hoistFlagsBeforePositionals(tc.in)
			if len(got) == 0 && len(tc.want) == 0 {
				return
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("hoistFlagsBeforePositionals(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
