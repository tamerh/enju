package layout

import (
	"strings"
	"testing"
)

// TestBigfilesBranchDir pins the per-branch bigfiles path
// schema. Wrappers that resolve writes_artifacts(track:false)
// paths and the readers (enju_list_untracked_artifacts,
// downstream presence checks) all derive from this — a
// regression here would split the producer's location from
// the consumer's location and untracked artifacts would
// silently disappear from the index.
func TestBigfilesBranchDir(t *testing.T) {
	cases := []struct {
		name   string
		branch string
		want   string
	}{
		{"main branch", "main", ".enju/bigfiles/main"},
		{"feature branch", "feature-x", ".enju/bigfiles/feature-x"},
		{"slashed branch", "user/work", ".enju/bigfiles/user/work"},
		{"empty defaults to main", "", ".enju/bigfiles/main"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := BigfilesBranchDir(tc.branch)
			if got != tc.want {
				t.Errorf("BigfilesBranchDir(%q) = %q, want %q", tc.branch, got, tc.want)
			}
			// .enju/-prefix invariant: bigfiles must live under
			// the gitignored umbrella so the operator's project
			// tree (which IS the worktree post-Phase-8) doesn't
			// see them as untracked.
			if !strings.HasPrefix(got, ".enju/") {
				t.Errorf("expected .enju/ prefix, got %q", got)
			}
		})
	}
}
