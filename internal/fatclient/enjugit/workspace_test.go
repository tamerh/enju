package enjugit

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// nullLogger discards everything.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initBareForWorkspaceTest creates a bare git repo with one
// initial commit on main. Returns the bare path. Thin wrapper
// around gittest.InitBareWithSeed kept under this name so
// existing call sites across enjugit tests don't churn.
func initBareForWorkspaceTest(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	gittest.InitBareWithSeed(t, bare)
	return bare
}

func TestNewWorkspace_DefaultsToHome(t *testing.T) {
	// Use an explicit dir so we don't pollute ~ during tests.
	dir := t.TempDir()
	ws, err := NewWorkspace(dir, NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if ws.RootDir() != dir {
		t.Errorf("RootDir: got %s, want %s", ws.RootDir(), dir)
	}
}

func TestForProject_FreshClone(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := ws.ForProject(7, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if wf.ProjectID() != 7 {
		t.Errorf("ProjectID: got %d, want 7", wf.ProjectID())
	}
	// Cached: second call returns same handle.
	wf2, _ := ws.ForProject(7, bare)
	if wf != wf2 {
		t.Error("ForProject should cache by id")
	}
	if !ws.HasLocalClone(7) {
		t.Error("HasLocalClone should be true after ForProject")
	}
}

func TestForProject_NoSource_InitsLocal(t *testing.T) {
	// Empty remoteURL is the "solo / no-remote" project mode.
	// ForProject inits a local-only clone (with seed) so the
	// workflow is usable without an upstream. Callers can wire
	// origin later via SetRemote.
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(7, "")
	if err != nil {
		t.Fatalf("ForProject with empty remoteURL: expected local-init, got %v", err)
	}
	if wf == nil {
		t.Fatal("ForProject returned nil workflow")
	}
	if !ws.HasLocalClone(7) {
		t.Error("HasLocalClone should be true after local-init")
	}
}

func TestOpenView_CloneNotFound(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	_, err := ws.OpenView(7)
	if !errors.Is(err, ErrCloneNotFound) {
		t.Errorf("expected ErrCloneNotFound, got %v", err)
	}
}

// TestOpenView_DoesNotInitOnMissing pins the read-side contract:
// when no clone exists, OpenView must REFUSE to silently init a
// stub at <rootDir>/<id>. A buggy build's ForProject(id, "") could
// PlainInit a numeric-form stub that findProjectDir would then
// return as if it were the real clone. enjugit's OpenView only
// reads, so the invariant is enforced by code structure; this test
// pins it so any future "open-or-init" convenience accidentally
// added to the read path fails loudly.
func TestOpenView_DoesNotInitOnMissing(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root, NewProductionConventions(), WithLogger(nullLogger()))
	if _, err := ws.OpenView(42); !errors.Is(err, ErrCloneNotFound) {
		t.Fatalf("expected ErrCloneNotFound, got %v", err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() == "42" || e.Name() == "0" {
			t.Errorf("OpenView created stub directory %q — must never init", e.Name())
		}
	}
}

// TestProjectDirLocked_PrefersSlugOverNumeric covers the tie-break
// in projectDirLocked's scan path: when both slug-form (e.g.
// "webui-toy-1") and numeric ("1") directories live under rootDir,
// the slug-form wins. Without this rule, alphabetical os.ReadDir
// returns "1" before "webui-toy-1" and a buggy build's leftover
// numeric stub would shadow the real clone.
func TestProjectDirLocked_PrefersSlugOverNumeric(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root, NewProductionConventions(), WithLogger(nullLogger()))

	// Plant the slug-form clone (with .git so projectDirLocked's
	// IsDir check would accept it; though scan only looks at name
	// suffix, we keep .git for parity with project package's setup).
	wantSlug := filepath.Join(root, "webui-toy-1")
	if err := os.MkdirAll(filepath.Join(wantSlug, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Plant a numeric-form orphan stub.
	stub := filepath.Join(root, "1")
	if err := os.MkdirAll(filepath.Join(stub, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	if got := ws.ProjectDir(1); got != wantSlug {
		t.Errorf("tie-break failed: got %q, want %q (slug-form should beat numeric)", got, wantSlug)
	}
}

// TestProjectDirLocked_NumericFallback verifies the legacy-compat
// fallback: when ONLY the numeric form exists (no slug-id sibling),
// projectDirLocked returns it. Otherwise pre-slug-rename projects
// would silently lose their on-disk clone.
func TestProjectDirLocked_NumericFallback(t *testing.T) {
	root := t.TempDir()
	ws, _ := NewWorkspace(root, NewProductionConventions(), WithLogger(nullLogger()))
	dir := filepath.Join(root, "1")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ws.ProjectDir(1); got != dir {
		t.Errorf("numeric fallback broke: got %q, want %q", got, dir)
	}
}

func TestOpenOrLazyClone_LazyClonesWhenMissing(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	v, err := ws.OpenOrLazyClone(7, bare)
	if err != nil {
		t.Fatalf("OpenOrLazyClone: %v", err)
	}
	if v.ProjectID() != 7 {
		t.Errorf("ProjectID: got %d, want 7", v.ProjectID())
	}
}

func TestOpenOrLazyClone_NoSource(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	_, err := ws.OpenOrLazyClone(7, "")
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("expected ErrNoCloneSource, got %v", err)
	}
}

func TestProductionBranchName(t *testing.T) {
	convs := NewProductionConventions()
	got := convs.BranchName(2, "build", "develop_a", "", 3)
	want := "2-build/develop_a/iter-3"
	if got != want {
		t.Errorf("BranchName: got %q, want %q", got, want)
	}
	// With instance key.
	got = convs.BranchName(1, "build", "review", "module-x", 1)
	want = "1-build/module-x/review/iter-1"
	if got != want {
		t.Errorf("BranchName with instance: got %q, want %q", got, want)
	}
}

func TestProductionDiskLayout(t *testing.T) {
	convs := NewProductionConventions()
	if got := convs.DiskLayout.BarePath("/proj"); got != "/proj/enju/.bare.git" {
		t.Errorf("BarePath: got %q", got)
	}
	if got := convs.DiskLayout.BotClonePath("/proj", "alice"); got != "/proj/.enju/bots/alice/worktree" {
		t.Errorf("BotClonePath: got %q", got)
	}
	if got := convs.DiskLayout.OperatorClonePath("/proj"); got != "/proj/enju/.clone" {
		t.Errorf("OperatorClonePath: got %q", got)
	}

	// ProjectRoot reverses the clone-suffix conventions so the
	// per-project trace log can be a single file across operator
	// + bots. Each input shape must round-trip back to the project
	// root the other constructors started from. Phase 4a moved
	// bot clones under .enju/bots/<name>/worktree/.
	cases := []struct{ in, want string }{
		{"/proj/enju/.clone", "/proj"},
		{"/proj/.enju/bots/alice/worktree", "/proj"},
		{"/proj/.enju/bots/reviewer-bot-1/worktree", "/proj"},
		{"/proj", "/proj"}, // autoLocal — workDir is already root
		{"/some/random/path", "/some/random/path"},
		{"", ""},
	}
	for _, c := range cases {
		if got := convs.DiskLayout.ProjectRoot(c.in); got != c.want {
			t.Errorf("ProjectRoot(%q): got %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLeaveProjectRemovesClone mirrors the project-side test that
// moved here when service.LocalLeaveProject started routing the
// disk-wipe through enjugit. ForProject → LeaveProject → ForProject
// should clone, wipe, and re-clone the same dir.
func TestLeaveProjectRemovesClone(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := ws.ForProject(70, bare)
	if err != nil {
		t.Fatalf("first clone: %v", err)
	}
	workDir := wf.WorkDir()
	if _, err := os.Stat(workDir); err != nil {
		t.Fatalf("expected clone dir to exist: %v", err)
	}
	if !ws.HasLocalClone(70) {
		t.Fatal("HasLocalClone should be true before leave")
	}

	if err := ws.LeaveProject(70); err != nil {
		t.Fatalf("LeaveProject: %v", err)
	}
	if _, err := os.Stat(workDir); !os.IsNotExist(err) {
		t.Errorf("expected clone dir to be gone, stat err: %v", err)
	}
	if ws.HasLocalClone(70) {
		t.Error("HasLocalClone should be false after leave")
	}

	// Leaving a project that was never opened is a no-op.
	if err := ws.LeaveProject(999); err != nil {
		t.Errorf("LeaveProject on unknown project: %v", err)
	}

	// Next ForProject re-clones into the same dir.
	wf2, err := ws.ForProject(70, bare)
	if err != nil {
		t.Fatalf("reclone after leave: %v", err)
	}
	if wf2.WorkDir() != workDir {
		t.Errorf("expected same work dir after reclone, got %s vs %s", wf2.WorkDir(), workDir)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"hello":          "hello",
		"Hello World":    "hello-world",
		"foo!bar@baz":    "foo-bar-baz",
		"   trim   me  ": "trim-me",
		"":               "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q): got %q, want %q", in, want, got)
		}
	}
}
