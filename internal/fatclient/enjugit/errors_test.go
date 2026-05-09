package enjugit

import (
	"errors"
	"strings"
	"testing"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitv6"
)

// TestGitOpError_FormatsContext locks the on-disk shape of the
// error message. Operators see this in cascade output, trace
// logs, and CI output — the format is a contract. Three
// fields (branch, workdir, origin) appear inside `{...}` in
// declaration order, comma-separated. Empty fields are omitted
// so a non-branch-scoped op like "fetch" doesn't render
// "branch=" garbage.
func TestGitOpError_FormatsContext(t *testing.T) {
	cases := []struct {
		name string
		err  *gitOpError
		want []string // substrings that must appear, in order
	}{
		{
			name: "all fields",
			err: &gitOpError{
				Op:        "push",
				Branch:    "main",
				WorkDir:   "/work/proj",
				RemoteURL: "/work/proj/enju/.bare.git",
				Cause:     errors.New("rejected"),
			},
			want: []string{"push ", "branch=main", "workdir=/work/proj", "origin=/work/proj/enju/.bare.git", ": rejected"},
		},
		{
			name: "no branch (whole-clone op)",
			err: &gitOpError{
				Op:        "fetch",
				WorkDir:   "/work/proj",
				RemoteURL: "/remote/bare.git",
				Cause:     errors.New("no route to host"),
			},
			want: []string{"fetch ", "workdir=/work/proj", "origin=/remote/bare.git"},
		},
		{
			name: "bare clone (no remote)",
			err: &gitOpError{
				Op:      "checkout branch",
				Branch:  "topic/x",
				WorkDir: "/work/proj",
				Cause:   errors.New("ref not found"),
			},
			want: []string{"checkout branch ", "branch=topic/x", "workdir=/work/proj"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := tc.err.Error()
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("missing %q in:\n  %s", w, got)
				}
			}
			// "branch=" must NOT appear when the field is empty.
			if tc.err.Branch == "" && strings.Contains(got, "branch=") {
				t.Errorf("empty branch leaked into message:\n  %s", got)
			}
		})
	}
}

// TestGitOpError_PreservesSentinel ensures errors.Is walks
// through the wrap so callers' sentinel checks keep working
// after we layer the gitOpError on top. A regression here
// would silently break the entire submit pipeline's error
// routing (ErrPushNonFF → conflict spawn, ErrCommitNotFound
// → 404, etc.).
func TestGitOpError_PreservesSentinel(t *testing.T) {
	wrapped := &gitOpError{
		Op:    "push",
		Cause: translateGitError("push", git.ErrPushNonFF),
	}
	if !errors.Is(wrapped, ErrPushNonFF) {
		t.Errorf("errors.Is(wrap, ErrPushNonFF) = false, want true")
	}
}
