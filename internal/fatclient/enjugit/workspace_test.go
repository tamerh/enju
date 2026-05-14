package enjugit

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// newTestWorkspaceWithProject constructs a fresh Workspace with a
// projectreg.Registry attached and pre-upserts the given project
// at a freshly-created subdir. Returns the workspace, the
// registry, and the project's path. Post-NDW.2 the workspace
// requires a registry; this helper collapses the boilerplate
// every test would otherwise repeat.
//
// projectreg.Registry.Get filters entries whose LocalPath no
// longer exists on disk (treats them as stale), so the helper
// MkdirAll's the project path BEFORE upserting — otherwise the
// registry would silently report the entry as missing and the
// test would see ErrProjectNotRegistered. Mirrors the production
// invariant from EagerInitProjectClone: the operator's chosen
// dir already exists when the handler upserts.
func newTestWorkspaceWithProject(t *testing.T, projectID int64) (*Workspace, *projectreg.Registry, string) {
	t.Helper()
	wsRoot := t.TempDir()
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	projectPath := filepath.Join(wsRoot, "p")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	if err := reg.Upsert(projectreg.Entry{ID: projectID, LocalPath: projectPath}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := NewWorkspace(wsRoot, NewProductionConventions(),
		WithLogger(nullLogger()), WithRegistry(reg))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws, reg, projectPath
}

// newWorkspaceForIDs builds a Workspace + Registry where every
// given ID is pre-mapped to a freshly-created subdir under the
// workspace root. Used by tests that previously relied on the
// implicit "scan rootDir, default to numeric form" path. Returns
// the workspace and the registry — callers occasionally need to
// upsert additional rows (e.g. a second project across a
// different writer's clone).
//
// Multiple IDs all map to the SAME subdir (wsRoot/p) — the
// pre-NDW.2 numeric-form layout used per-ID dirs, but every
// existing call site uses one project per workspace, so the
// collapsed shape is faithful enough and saves the helper from
// taking per-ID path arguments. If a test needs distinct dirs
// per ID it can call Upsert directly on the returned registry.
func newWorkspaceForIDs(t *testing.T, ids ...int64) (*Workspace, *projectreg.Registry) {
	t.Helper()
	wsRoot := t.TempDir()
	reg := projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))
	projectPath := filepath.Join(wsRoot, "p")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatalf("mkdir project path: %v", err)
	}
	for _, id := range ids {
		if err := reg.Upsert(projectreg.Entry{
			ID:        id,
			LocalPath: projectPath,
		}); err != nil {
			t.Fatalf("registry upsert id=%d: %v", id, err)
		}
	}
	ws, err := NewWorkspace(wsRoot, NewProductionConventions(),
		WithLogger(nullLogger()), WithRegistry(reg))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	return ws, reg
}

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

func TestNewWorkspace_ExplicitDir(t *testing.T) {
	dir := t.TempDir()
	ws, err := NewWorkspace(dir, NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if ws.RootDir() != dir {
		t.Errorf("RootDir: got %s, want %s", ws.RootDir(), dir)
	}
}

// TestForProject_NotRegistered pins the post-NDW.2 contract:
// without a registry entry, ForProject errors with
// ErrProjectNotRegistered. There is no scan-rootDir fallback —
// every project is path-anchored via projectreg.
func TestForProject_NotRegistered(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(),
		WithLogger(nullLogger()),
		WithRegistry(projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ForProject(7, bare); !errors.Is(err, ErrProjectNotRegistered) {
		t.Errorf("ForProject without registry entry: got %v, want ErrProjectNotRegistered", err)
	}
}

// TestForProject_NoRegistryAttached covers the programming-error
// path: Workspace constructed without WithRegistry should refuse
// to resolve any project ID.
func TestForProject_NoRegistryAttached(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.ForProject(7, bare); !errors.Is(err, ErrProjectNotRegistered) {
		t.Errorf("ForProject with no registry: got %v, want ErrProjectNotRegistered", err)
	}
}

func TestForProject_FreshClone(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _, projectPath := newTestWorkspaceWithProject(t, 7)
	wf, err := ws.ForProject(7, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if wf.ProjectID() != 7 {
		t.Errorf("ProjectID: got %d, want 7", wf.ProjectID())
	}
	if wf.WorkDir() != projectPath {
		t.Errorf("WorkDir: got %s, want %s (registry resolution)", wf.WorkDir(), projectPath)
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
	// ForProject inits a local-only clone (with seed) at the
	// registered path so the workflow is usable without an upstream.
	ws, _, _ := newTestWorkspaceWithProject(t, 7)
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
	ws, _, _ := newTestWorkspaceWithProject(t, 7)
	_, err := ws.OpenView(7)
	if !errors.Is(err, ErrCloneNotFound) {
		t.Errorf("expected ErrCloneNotFound, got %v", err)
	}
}

func TestOpenView_NotRegistered(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(),
		WithLogger(nullLogger()),
		WithRegistry(projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.OpenView(7); !errors.Is(err, ErrProjectNotRegistered) {
		t.Errorf("OpenView without registry entry: got %v, want ErrProjectNotRegistered", err)
	}
}

// TestOpenView_DoesNotInitOnMissing pins the read-side contract:
// when the registered path exists but contains no .git, OpenView
// must NOT create one. The post-NDW.2 invariant is that adoption
// creates the .git (via enju_create_project); read-only verbs
// only consume it.
func TestOpenView_DoesNotInitOnMissing(t *testing.T) {
	ws, _, projectPath := newTestWorkspaceWithProject(t, 42)
	if _, err := ws.OpenView(42); !errors.Is(err, ErrCloneNotFound) {
		t.Fatalf("expected ErrCloneNotFound, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(projectPath, ".git")); !os.IsNotExist(err) {
		t.Errorf("OpenView materialized a .git inside %q — must never init", projectPath)
	}
}

func TestOpenOrLazyClone_NotRegistered(t *testing.T) {
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(),
		WithLogger(nullLogger()),
		WithRegistry(projectreg.Open(filepath.Join(t.TempDir(), "projects.json"))))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ws.OpenOrLazyClone(7, "remote-url-ignored"); !errors.Is(err, ErrProjectNotRegistered) {
		t.Errorf("OpenOrLazyClone without registry entry: got %v, want ErrProjectNotRegistered", err)
	}
}

// TestOpenOrLazyClone_RegisteredButNoClone covers the post-NDW.2
// "no silent materialization" semantics: even when the project IS
// registered, OpenOrLazyClone errors with ErrCloneNotFound when no
// .git exists at the registered path. Adoption goes through
// enju_create_project, not a silent webui-side clone.
func TestOpenOrLazyClone_RegisteredButNoClone(t *testing.T) {
	ws, _, _ := newTestWorkspaceWithProject(t, 7)
	if _, err := ws.OpenOrLazyClone(7, "ignored"); !errors.Is(err, ErrCloneNotFound) {
		t.Errorf("OpenOrLazyClone: got %v, want ErrCloneNotFound", err)
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

	// ProjectRoot is the identity (post-Phase-8 the operator's
	// adopted dir IS the working tree). Empty input collapses
	// to empty; everything else returns Clean(workDir).
	cases := []struct{ in, want string }{
		{"/proj", "/proj"},
		{"/some/random/path", "/some/random/path"},
		{"/proj/", "/proj"}, // Clean strips trailing slash
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
// disk-wipe through enjugit. ForProject → LeaveProject removes
// the on-disk clone.
//
// Post-NDW.2 a re-adoption (re-clone after Leave) requires the
// operator to re-register via enju_create_project: registry.Get
// filters entries whose LocalPath no longer exists on disk, so
// the second ForProject without re-registration errors with
// ErrProjectNotRegistered. The pre-NDW.2 "same workspace, same
// dir, reclone for free" semantics is intentionally gone — the
// registry is the source of truth for what projects this
// machine is "in."
func TestLeaveProjectRemovesClone(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, reg, projectPath := newTestWorkspaceWithProject(t, 70)
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

	// Leaving a project that was never registered is a no-op.
	if err := ws.LeaveProject(999); err != nil {
		t.Errorf("LeaveProject on unknown project: %v", err)
	}

	// Without re-registration the registry entry's stale LocalPath
	// filters out — ForProject errors instead of silently re-
	// materializing at the wiped path.
	if _, err := ws.ForProject(70, bare); !errors.Is(err, ErrProjectNotRegistered) {
		t.Errorf("ForProject after leave (no re-register): got %v, want ErrProjectNotRegistered", err)
	}

	// Operator re-adopts at the same path — mirrors
	// enju_create_project's Upsert-then-ForProject sequence.
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := reg.Upsert(projectreg.Entry{ID: 70, LocalPath: projectPath}); err != nil {
		t.Fatalf("re-upsert: %v", err)
	}
	wf2, err := ws.ForProject(70, bare)
	if err != nil {
		t.Fatalf("reclone after re-register: %v", err)
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
