package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

func TestParseParamsArg(t *testing.T) {
	cases := []struct {
		in   string
		want map[string]string
		err  bool
	}{
		{"", nil, false},
		{"gene=TP53", map[string]string{"gene": "TP53"}, false},
		{"gene=TP53, effort=high", map[string]string{"gene": "TP53", "effort": "high"}, false},
		{"  gene = TP53  ", map[string]string{"gene": "TP53"}, false},
		{"gene", nil, true},
		{"=value", nil, true},
	}
	for _, c := range cases {
		got, err := parseParamsArg(c.in)
		if c.err {
			if err == nil {
				t.Errorf("parseParamsArg(%q): expected error, got %v", c.in, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("parseParamsArg(%q): unexpected error %v", c.in, err)
			continue
		}
		if len(got) != len(c.want) {
			t.Errorf("parseParamsArg(%q): got %v, want %v", c.in, got, c.want)
			continue
		}
		for k, v := range c.want {
			if got[k] != v {
				t.Errorf("parseParamsArg(%q)[%s] = %v, want %v", c.in, k, got[k], v)
			}
		}
	}
}

// TestPickContainingEntryPrefersDeepest exercises the nested-
// project case: when /home/foo is registered as project 1 AND
// /home/foo/nested is registered as project 2, a file under
// nested/ should resolve to project 2.
func TestPickContainingEntryPrefersDeepest(t *testing.T) {
	now := time.Now()
	entries := []projectreg.Entry{
		{ID: 1, LocalPath: "/home/foo", LastTouched: now},
		{ID: 2, LocalPath: "/home/foo/nested", LastTouched: now},
		{ID: 3, LocalPath: "/elsewhere", LastTouched: now},
	}
	got := pickContainingEntry(entries, "/home/foo/nested/workflow.yaml")
	if got == nil || got.ID != 2 {
		t.Fatalf("expected entry 2, got %+v", got)
	}
}

func TestPickContainingEntryNoMatch(t *testing.T) {
	entries := []projectreg.Entry{
		{ID: 1, LocalPath: "/home/foo"},
	}
	if got := pickContainingEntry(entries, "/elsewhere/file"); got != nil {
		t.Fatalf("expected nil, got %+v", got)
	}
}

// TestProjectRootCandidateFindsGit places a workflow under a
// directory tree with .git two levels up; the candidate should
// walk up to the git root rather than picking the workflow's
// immediate parent.
func TestProjectRootCandidateFindsGit(t *testing.T) {
	dir := t.TempDir()
	deep := filepath.Join(dir, "a", "b")
	if err := os.MkdirAll(deep, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	got := projectRootCandidate(filepath.Join(deep, "wf.yaml"))
	if got != dir {
		t.Fatalf("expected %s, got %s", dir, got)
	}
}

func TestProjectRootCandidateFallsBackToParent(t *testing.T) {
	dir := t.TempDir()
	got := projectRootCandidate(filepath.Join(dir, "wf.yaml"))
	if got != dir {
		t.Fatalf("expected fallback %s, got %s", dir, got)
	}
}

func TestRunIdentityFromCreateResponse(t *testing.T) {
	data := []byte(`{"seq":3,"id":42,"branch":"foo-3"}`)
	seq, id := runIdentityFromCreateResponse(data)
	if seq != 3 || id != 42 {
		t.Fatalf("got seq=%d id=%d, want 3,42", seq, id)
	}
}

func TestRunIdentityFromCreateResponseHandlesGarbage(t *testing.T) {
	if seq, id := runIdentityFromCreateResponse([]byte("not json")); seq != 0 || id != 0 {
		t.Fatalf("expected zeros for garbage, got %d %d", seq, id)
	}
}

