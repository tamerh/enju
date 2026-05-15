package main

import (
	"regexp"
	"testing"
)

// usernamePattern mirrors store.usernameRe (the rule the coord
// applies when auto-registering a model name). nonLLMModelName's
// output MUST always satisfy it — the original `script:<base>`
// bug was a value that didn't, so the non-LLM bot could never
// claim. Pinning the invariant here means a future tweak to the
// derivation can't silently reintroduce an unregisterable name.
var usernamePattern = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

func TestNonLLMModelName(t *testing.T) {
	cases := []struct {
		handler string
		want    string
	}{
		{"./bin/lint-bot.sh", "script-lint-bot-sh"},
		{"bin/lint-bot.sh", "script-lint-bot-sh"},
		{"lint-bot.sh", "script-lint-bot-sh"},
		{"/usr/local/bin/Run_Linter.PY", "script-run-linter-py"},
		{"weird:::name...sh", "script-weird-name-sh"},
		{"--leading-and-trailing--", "script-leading-and-trailing"},
		{"/tmp/.x.", "script-x"},
		{"", "script-nonllm"},
		{"!!!", "script-nonllm"},
		{"   ", "script-nonllm"},
	}
	for _, tc := range cases {
		t.Run(tc.handler, func(t *testing.T) {
			got := nonLLMModelName(tc.handler)
			if got != tc.want {
				t.Errorf("nonLLMModelName(%q) = %q, want %q", tc.handler, got, tc.want)
			}
			// The load-bearing invariant: whatever we produce
			// MUST be coord-registerable (username charset).
			if !usernamePattern.MatchString(got) {
				t.Errorf("nonLLMModelName(%q) = %q is not a valid coord model/username name", tc.handler, got)
			}
		})
	}
}
