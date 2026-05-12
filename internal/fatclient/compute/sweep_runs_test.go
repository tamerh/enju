package compute

import (
	"os"
	"path/filepath"
	"testing"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
)

// TestSweepRunStateDirs_RemovesNonAliveOnly is the contract test:
// every per-run state dir under .enju/runs/ whose seq isn't in
// aliveSeqs gets removed; the others stay.
func TestSweepRunStateDirs_RemovesNonAliveOnly(t *testing.T) {
	projectRoot := t.TempDir()
	mustMkPerRunDir(t, projectRoot, 1, "alpha")
	mustMkPerRunDir(t, projectRoot, 2, "beta")
	mustMkPerRunDir(t, projectRoot, 3, "gamma")

	alive := map[int]bool{2: true}
	n, err := SweepRunStateDirs(projectRoot, alive)
	if err != nil {
		t.Fatalf("SweepRunStateDirs: %v", err)
	}
	if n != 2 {
		t.Errorf("expected 2 dirs removed, got %d", n)
	}
	assertPerRunDirGone(t, projectRoot, 1, "alpha")
	assertPerRunDirPresent(t, projectRoot, 2, "beta")
	assertPerRunDirGone(t, projectRoot, 3, "gamma")
}

// TestSweepRunStateDirs_NoEACCESOnSimpleRmRf pins TP53 Bug 3
// resolution: a per-run snapshot dir filled with files at default
// permissions (no chmod step) gets removed cleanly via os.RemoveAll
// — no permission-denied. Catches a regression that re-introduces
// chmod-readonly enforcement.
func TestSweepRunStateDirs_NoEACCESOnSimpleRmRf(t *testing.T) {
	projectRoot := t.TempDir()
	runDir := mustMkPerRunDir(t, projectRoot, 7, "snap-test")
	// Seed a non-trivial subtree underneath: nested dirs + executable
	// file + plain file. This is the shape MaterializeRunRepo produces;
	// before the redesign, ChmodSnapshotReadOnly would have made parent
	// dirs 0555 and any cleanup would hit EACCES.
	if err := os.MkdirAll(filepath.Join(runDir, "snapshot", "scripts"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "snapshot", "data.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(runDir, "snapshot", "scripts", "run.sh"), []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	// Empty alive set → seq 7 is terminal → eligible for sweep.
	n, err := SweepRunStateDirs(projectRoot, map[int]bool{})
	if err != nil {
		t.Fatalf("SweepRunStateDirs returned error (expected clean rm -rf): %v", err)
	}
	if n != 1 {
		t.Errorf("expected 1 dir removed, got %d", n)
	}
	assertPerRunDirGone(t, projectRoot, 7, "snap-test")
}

// TestSweepRunStateDirs_MissingRootIsNoOp pins the soft-fail
// behavior: a project that has never run anything has no
// .enju/runs/ yet; the sweep must succeed silently.
func TestSweepRunStateDirs_MissingRootIsNoOp(t *testing.T) {
	projectRoot := t.TempDir()
	n, err := SweepRunStateDirs(projectRoot, nil)
	if err != nil {
		t.Errorf("missing .enju/runs/ should be no-op, got: %v", err)
	}
	if n != 0 {
		t.Errorf("missing root: expected 0 removed, got %d", n)
	}
}

// TestSweepRunStateDirs_EmptyProjectRootIsNoOp pins the
// no-workspace path (MCP-client-only mode, fakes).
func TestSweepRunStateDirs_EmptyProjectRootIsNoOp(t *testing.T) {
	if n, err := SweepRunStateDirs("", nil); err != nil || n != 0 {
		t.Errorf("empty projectRoot: want (0,nil); got (%d,%v)", n, err)
	}
}

// TestSweepRunStateDirs_IgnoresUnrelatedEntries makes sure stray
// files or non-conforming dirs under .enju/runs/ don't get
// touched. Defends against future siblings (.enju/runs/scratch/
// or similar) being clobbered by name shape only.
func TestSweepRunStateDirs_IgnoresUnrelatedEntries(t *testing.T) {
	projectRoot := t.TempDir()
	mustMkPerRunDir(t, projectRoot, 1, "ok")

	runsRoot := filepath.Join(projectRoot, corelayout.RunStateRunsRoot())
	// A file at the runs/ level — never matched.
	if err := os.WriteFile(filepath.Join(runsRoot, "note.txt"), []byte("meta"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A dir without a numeric prefix — never matched.
	if err := os.MkdirAll(filepath.Join(runsRoot, "shared-cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	// A dir whose prefix isn't an int — never matched.
	if err := os.MkdirAll(filepath.Join(runsRoot, "abc-foo"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := SweepRunStateDirs(projectRoot, map[int]bool{})
	if err != nil {
		t.Fatalf("SweepRunStateDirs: %v", err)
	}
	// Unrelated entries must survive.
	if _, err := os.Stat(filepath.Join(runsRoot, "note.txt")); err != nil {
		t.Errorf("note.txt was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runsRoot, "shared-cache")); err != nil {
		t.Errorf("shared-cache/ was removed: %v", err)
	}
	if _, err := os.Stat(filepath.Join(runsRoot, "abc-foo")); err != nil {
		t.Errorf("abc-foo/ was removed: %v", err)
	}
	// Numeric-prefixed dir was removed.
	assertPerRunDirGone(t, projectRoot, 1, "ok")
}

func TestParseRunSeqPrefix(t *testing.T) {
	cases := []struct {
		in     string
		wantOK bool
		want   int
	}{
		{"1-foo", true, 1},
		{"42-some-slug", true, 42},
		{"abc-foo", false, 0},
		{"-foo", false, 0},
		{"123", false, 0},
		{"", false, 0},
		{"0-zero", false, 0}, // 0 isn't a legal run seq
	}
	for _, c := range cases {
		got, ok := parseRunSeqPrefix(c.in)
		if ok != c.wantOK || got != c.want {
			t.Errorf("parseRunSeqPrefix(%q) = (%d, %v); want (%d, %v)", c.in, got, ok, c.want, c.wantOK)
		}
	}
}

func mustMkPerRunDir(t *testing.T, projectRoot string, seq int, slug string) string {
	t.Helper()
	p := filepath.Join(projectRoot, corelayout.RunStateDir(seq, slug))
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", p, err)
	}
	return p
}

func assertPerRunDirGone(t *testing.T, projectRoot string, seq int, slug string) {
	t.Helper()
	p := filepath.Join(projectRoot, corelayout.RunStateDir(seq, slug))
	if _, err := os.Stat(p); !os.IsNotExist(err) {
		t.Errorf("expected %s to be removed, got err=%v", p, err)
	}
}

func assertPerRunDirPresent(t *testing.T, projectRoot string, seq int, slug string) {
	t.Helper()
	p := filepath.Join(projectRoot, corelayout.RunStateDir(seq, slug))
	if _, err := os.Stat(p); err != nil {
		t.Errorf("expected %s to remain, got err=%v", p, err)
	}
}
