package engine

// Pin the split between strict (literal) and relaxed (declaration)
// artifact-path validators. The strict variant runs at submit
// time on concrete paths the citizen wrote; the relaxed variant
// runs at parse time on declared writes_artifacts entries which
// may be globs or directories. Both share the safety floor
// (no leading `/`, no `..`, no `.git/`, no `.enju/`, no `enju/`).

import (
	"strings"
	"testing"
)

func TestValidateArtifactPath_StrictRejectsPatterns(t *testing.T) {
	cases := []struct {
		path string
		want string // expected substring of error message; "" means accept
	}{
		// Accepts (concrete literal paths).
		{"src/server.go", ""},
		{"out/file.bin", ""},

		// Rejects: safety floor.
		{"", "empty"},
		{"/abs/path", "leading"},
		{"../escape", ".."},
		{".git", ".git"},
		{".enju/runs/x", ".enju/"}, // post-Phase-8.h: audit lives under .enju/ but is enju-managed; declaring it would clobber state
		{"enju/state.txt", "enju/"}, // C1: bare enju/ (template-bundle root) is reserved too — was enforced nowhere
		{"enju", "enju/"},

		// Rejects: glob characters not allowed in a literal path.
		{"src/*.go", "glob"},
		{"cmd/?/main.go", "glob"},
		{"src/[abc].go", "glob"},
	}
	for _, tc := range cases {
		err := ValidateArtifactPath(tc.path)
		if tc.want == "" && err != nil {
			t.Errorf("ValidateArtifactPath(%q): unexpected error %v", tc.path, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("ValidateArtifactPath(%q): got %v, want error containing %q", tc.path, err, tc.want)
		}
	}
}

func TestValidateArtifactDeclaration_AcceptsPatternsRejectsBadPaths(t *testing.T) {
	cases := []struct {
		path string
		want string // expected error substring; "" means accept
	}{
		// Patterns accepted.
		{"src/api/", ""},
		{"src/api/*.go", ""},
		{"cmd/?/main.go", ""},
		{"src/[abc].go", ""},
		{"src/server.go", ""}, // literals still valid
		{"src/api/sub/", ""},  // nested directory

		// Safety floor still enforced — patterns don't bypass.
		{"", "empty"},
		{"/abs/path", "leading"},
		{"../escape/*.go", ".."},
		{".git/", ".git"},                // trailing slash doesn't excuse infra
		{".enju/", ".enju/"},                // declaring .enju/ would sweep our state
		{".enju/runs/*/result.md", ".enju/"}, // ditto for sub-paths
		{"enju/", "enju/"},                  // C1: bare enju/ reserved (template bundles live there)
		{"enju/templates/*.yaml", "enju/"},  // ditto for sub-paths
	}
	for _, tc := range cases {
		err := ValidateArtifactDeclaration(tc.path)
		if tc.want == "" && err != nil {
			t.Errorf("ValidateArtifactDeclaration(%q): unexpected error %v", tc.path, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("ValidateArtifactDeclaration(%q): got %v, want error containing %q", tc.path, err, tc.want)
		}
	}
}
