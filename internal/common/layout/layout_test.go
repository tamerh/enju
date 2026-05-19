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

// TestIsRunResultTrailPath pins the bounded-snapshot predicate
// (B5). The materializer relies on this to drop prior runs'
// force-committed result trails while keeping the recipe snapshot
// and ordinary source — a regression either re-introduces the
// linear snapshot bloat or (worse) strips the recipe so scripts
// can't resolve.
func TestIsRunResultTrailPath(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		// Result trail under a per-run dir → excluded.
		{".enju/runs/1-prev/analyze/result.md", true},
		{".enju/runs/12-load-test/zz1/generate/metadata.json", true},
		{".enju/runs/3-foo/t1/script.log", true},
		{".enju/runs/3-foo/graph/dag.mmd", true},
		// Recipe snapshot → kept.
		{".enju/runs/2-cur/template-snapshot/enju.yaml", false},
		{".enju/runs/2-cur/template-snapshot/scripts/run.sh", false},
		// Ordinary source / non-.enju → kept.
		{"src/lib.go", false},
		{"enju.yaml", false},
		{".gitignore", false},
		{".enju/events/live.jsonl", false},
		// Bare file directly under .enju/runs/ (no per-run dir) → kept.
		{".enju/runs/README", false},
		// "./"-prefixed variant normalizes the same way.
		{"./.enju/runs/1-prev/analyze/result.md", true},
	}
	for _, tc := range cases {
		if got := IsRunResultTrailPath(tc.path); got != tc.want {
			t.Errorf("IsRunResultTrailPath(%q) = %v, want %v", tc.path, got, tc.want)
		}
	}
}
