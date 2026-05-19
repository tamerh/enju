package artifactpath

import (
	"strings"
	"testing"
)

// TestValidateLiteral pins the strict (concrete-path) safety
// floor used for reads_artifacts + submit-time written paths.
func TestValidateLiteral(t *testing.T) {
	cases := []struct {
		path string
		want string // expected substring of error; "" means accept
	}{
		// Accepted — natural repo paths.
		{"src/server.go", ""},
		{"out/file.bin", ""},
		{"results/phage/summary.txt", ""},

		// Safety floor.
		{"", "empty"},
		{"/etc/passwd", "leading"},
		{"../outside.txt", ".."},
		{"a/../../b", ".."},

		// Reserved prefixes (the C1 fix — `enju/` was enforced
		// nowhere before; `.enju/` and `.git/` only on create).
		{"enju/state.txt", "enju/"},
		{"enju", "enju/"},
		{".enju/runs/x", ".enju/"},
		{".enju", ".enju/"},
		{".git/config", ".git/"},
		{".git", ".git/"},
		{`.git\objects\x`, ".git/"}, // backslash normalization

		// Glob metacharacters are not allowed in a literal path.
		{"src/*.go", "glob"},
		{"cmd/?/main.go", "glob"},
		{"src/[abc].go", "glob"},
	}
	for _, tc := range cases {
		err := ValidateLiteral(tc.path)
		if tc.want == "" && err != nil {
			t.Errorf("ValidateLiteral(%q): unexpected error %v", tc.path, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("ValidateLiteral(%q): got %v, want substring %q", tc.path, err, tc.want)
		}
	}
}

// TestValidateDeclaration pins the relaxed (writes_artifacts)
// variant: globs + trailing-slash dirs allowed, safety floor
// (incl. the reserved-prefix C1 fix) still enforced.
func TestValidateDeclaration(t *testing.T) {
	cases := []struct {
		path string
		want string
	}{
		{"src/api/", ""},
		{"src/api/*.go", ""},
		{"cmd/?/main.go", ""},
		{"src/[abc].go", ""},
		{"out/result.bin", ""},

		{"", "empty"},
		{"/abs", "leading"},
		{"../escape/*.go", ".."},
		{"enju/templates/x", "enju/"},
		{"enju/", "enju/"},
		{".enju/", ".enju/"},
		{".enju/runs/*/result.md", ".enju/"},
		{".git/", ".git/"},
	}
	for _, tc := range cases {
		err := ValidateDeclaration(tc.path)
		if tc.want == "" && err != nil {
			t.Errorf("ValidateDeclaration(%q): unexpected error %v", tc.path, err)
		}
		if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
			t.Errorf("ValidateDeclaration(%q): got %v, want substring %q", tc.path, err, tc.want)
		}
	}
}
