package service

// Tests for FatClient construction. Particularly the
// adopted-project bridge: registry → project.externalDirs at
// New() time, so a fatclient process restarted after
// `enju_create_project path=/external/dir` can still resolve
// that project to its adopted location.

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// initRealClone plants a usable git clone at dir + commits one
// file so OpenExisting can read it. Mirrors the helper in
// workspace/openexisting_test.go so this test doesn't reach
// into the workspace package's test-only helpers.
func initRealClone(t *testing.T, dir string) {
	t.Helper()
	gittest.Init(t, dir)
	gittest.Commit(t, dir, "README.md", "# adopted\n", "seed")
}

// TestNew_BridgesRegistryToExternalDirs pins the cross-process
// adopted-project bug: pre-fix, after `enju_create_project path=/dir`
// the fatclient that ran the init had externalDirs[id] = /dir
// in memory, but a fresh fatclient process (e.g. `enju ui`
// started later) had an empty externalDirs and OpenExisting
// returned ErrCloneNotFound for that ID even though the
// registry on disk knew the path.
//
// Post-fix: New() walks the registry and registers each entry
// with the workspace as an external dir. Both processes see
// the same answer.
func TestNew_BridgesRegistryToExternalDirs(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	// Adopted project lives at /tmp/test-adopted-N (outside the
	// workspace root). Plant a real clone there so OpenExisting's
	// gogit.PlainOpen succeeds when the bridge is wired.
	tmp := t.TempDir()
	adoptedDir := filepath.Join(tmp, "external-dir")
	initRealClone(t, adoptedDir)

	// Persist the entry to a real on-disk registry — what
	// `enju_create_project path=` would have done in a previous fatclient
	// process.
	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{
		ID:        42,
		LocalPath: adoptedDir,
		Name:      "adopted-test",
	}); err != nil {
		t.Fatal(err)
	}

	// FRESH fatclient — Workspace starts with empty in-memory
	// externalDirs, simulating a process restart.
	wsRoot := filepath.Join(tmp, "workspaces")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}

	// Open a SECOND registry handle pointing at the same file —
	// what production does. (Each process opens its own handle.)
	reg2 := projectreg.Open(regPath)
	ws.AttachRegistry(reg2)
	_ = New(Config{WorkspaceRoot: ws.RootDir(), ProjectRegistry: reg2, Logger: logger})

	// With the registry attached to the opener, OpenExisting
	// resolves project 42 to its registered adopted dir.
	proj, err := ws.OpenExisting(42)
	if err != nil {
		t.Fatalf("OpenExisting(42) after fresh fatclient + registry-bridge: %v", err)
	}
	if proj.WorkDir() != adoptedDir {
		t.Errorf("WorkDir: got %q, want %q (bridge should have routed via externalDirs)", proj.WorkDir(), adoptedDir)
	}
}

// TestNew_NoRegistry_NoBridge confirms the bridge is a no-op
// when the registry is unconfigured. Tests and minimal
// embeddings should keep working as before.
func TestNew_NoRegistry_NoBridge(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	ws, err := enjugit.NewWorkspace(tmp, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	_ = New(Config{WorkspaceRoot:   ws.RootDir(), Logger: logger}) // no ProjectRegistry
	// Nothing was registered → OpenExisting on an arbitrary id
	// returns ErrCloneNotFound, exactly as without the bridge.
	_, err = ws.OpenExisting(99)
	if !errors.Is(err, enjugit.ErrCloneNotFound) {
		t.Errorf("expected ErrCloneNotFound, got %v", err)
	}
}

// TestNew_RegistryStaleEntry_Skipped pins the no-double-stat
// behavior: Registry.List already filters stale paths, so a
// registered entry whose dir was deleted between sessions
// silently disappears (rather than the bridge faceplanting on
// it). Tests this by registering an entry pointing at a
// directory that doesn't exist; New must NOT register it.
func TestNew_RegistryStaleEntry_Skipped(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()

	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{
		ID:        77,
		LocalPath: filepath.Join(tmp, "deleted-already"),
		Name:      "stale",
	}); err != nil {
		t.Fatal(err)
	}

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, err := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	if err != nil {
		t.Fatal(err)
	}
	_ = New(Config{WorkspaceRoot:   ws.RootDir(), ProjectRegistry: projectreg.Open(regPath), Logger: logger})

	_, err = ws.OpenExisting(77)
	if !errors.Is(err, enjugit.ErrCloneNotFound) {
		t.Errorf("stale entry should be filtered; OpenExisting got: %v", err)
	}
}

// initRealCloneWithAuthor is initRealClone but with an
// explicit author. The vanilla helper relies on git reading
// the operator's ~/.gitconfig for user.name/user.email; tests
// that t.Setenv("HOME", tmp) hide that file and need to
// provide the author themselves. gittest.CommitAs supplies
// the identity via -c user.name / user.email so no global
// config is consulted.
func initRealCloneWithAuthor(t *testing.T, dir string) {
	t.Helper()
	gittest.Init(t, dir)
	gittest.CommitAs(t, dir, "README.md", "# adopted\n", "seed", "test", "test@example.com")
}

// (Removed: tests for the isManagedWorkspaceClone discriminator
// that filtered registry entries pointing inside the workspace
// root. With `enju_create_project path=` required, the registry
// can no longer hold a workspace-internal path, so the
// discriminator and its tests are unreachable.)

// (Removed: TestEnsureBotPushTarget_* tests. Phase 8 dropped
// the managed bare and the EnsureBotPushTarget helper that
// created it; solo single-machine projects now operate against
// the operator's own .git/ via plumbing, with no separate push
// target needed.)
