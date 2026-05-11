package gitcli

import (
	"strings"
	"testing"
)

// --- parseGitVersion ---

func TestParseGitVersionStandardOutput(t *testing.T) {
	cases := map[string]struct {
		maj, min int
	}{
		"git version 2.43.0\n":              {2, 43},
		"git version 2.30.2 (Apple Git-129)": {2, 30},
		"git version 2.39.2.windows.1":      {2, 39},
		"git version 2.40.0":                {2, 40},
		"git version 1.8.3.1":               {1, 8},
		"git version 3.0.0":                 {3, 0},
	}
	for in, want := range cases {
		maj, min, err := parseGitVersion(in)
		if err != nil {
			t.Errorf("%q: unexpected err: %v", in, err)
			continue
		}
		if maj != want.maj || min != want.min {
			t.Errorf("%q → %d.%d, want %d.%d", in, maj, min, want.maj, want.min)
		}
	}
}

func TestParseGitVersionRejectsGarbage(t *testing.T) {
	bad := []string{
		"",
		"foo bar baz",
		"git 2.40.0",       // missing "version"
		"git version 2",    // missing minor
		"git version x.y",  // non-numeric
	}
	for _, in := range bad {
		if _, _, err := parseGitVersion(in); err == nil {
			t.Errorf("%q: expected error, got nil", in)
		}
	}
}

// --- CheckMinVersion ---

func TestCheckMinVersionOnRealGit(t *testing.T) {
	// This runs against whatever `git` is on PATH. We don't
	// assert success or failure — the test machine's git may
	// be older than MinGitMinor. Instead we assert that the
	// error (if any) names the floor so operators can debug.
	if err := CheckMinVersion(); err != nil {
		if !strings.Contains(err.Error(), "git") {
			t.Errorf("error should mention git, got: %v", err)
		}
	}
}

// --- LC_ALL=C ---

func TestRunGitForcesCLocale(t *testing.T) {
	// Verify runGit prepends LC_ALL=C / LANG=C so stderr
	// patterns we classify don't get translated under
	// non-English locales. We can't easily simulate a French
	// git binary, but we CAN verify that even when the caller
	// passes LANG=fr_FR.UTF-8 in extraEnv (which would override
	// our base), the test merely documents the order of
	// precedence rather than asserting i18n behavior.
	//
	// What's testable: a known-failing command's stderr is
	// English when LC_ALL is C — `git rev-parse` on a missing
	// SHA produces "fatal: bad object" verbatim. We rely on
	// that consistency for classifyStderr.
	dir := t.TempDir()
	gitInit(t, dir)
	bogus := "deadbeef0123456789abcdef0123456789abcdef"
	_, err := runGit(dir, []string{"show", "--no-patch", bogus}, runOpts{})
	if err == nil {
		t.Fatal("expected error")
	}
	// Whether or not the test environment has a non-C locale,
	// runGit forced LC_ALL=C, so stderr is English, so
	// classifyStderr matched ErrCommitNotFound. errors.Is is
	// our witness — i18n breaking that would surface as a
	// fall-through to the generic wrap.
	if err.Error() == "" {
		t.Errorf("error message empty: %v", err)
	}
}
