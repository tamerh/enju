package service

// Tests for FatClient construction. Particularly the
// adopted-project bridge: registry → workspace.externalDirs at
// New() time, so a fatclient process restarted after `enju_init
// --path=/external/dir` can still resolve that project to its
// adopted location.

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"

	"github.com/enju-ai/enju/internal/fatclient/projectreg"
	"github.com/enju-ai/enju/internal/fatclient/workspace"
)

// initRealClone plants a usable git clone at dir + commits one
// file so OpenExisting (which uses gogit.PlainOpen) can read
// it. Mirrors the helper in workspace/openexisting_test.go so
// this test doesn't reach into the workspace package's test-
// only helpers.
func initRealClone(t *testing.T, dir string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# adopted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = wt.Add("README.md")
	if _, err := wt.Commit("seed", &gogit.CommitOptions{All: true}); err != nil {
		t.Fatal(err)
	}
}

// TestNew_BridgesRegistryToExternalDirs pins the cross-process
// adopted-project bug: pre-fix, after `enju_init --path=/dir`
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
	// `enju_init` would have done in a previous fatclient
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
	ws, err := workspace.NewWorkspace(wsRoot, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Open a SECOND registry handle pointing at the same file —
	// what production does. (Each process opens its own handle.)
	reg2 := projectreg.Open(regPath)
	_ = New(Config{Workspace: ws, ProjectRegistry: reg2, Logger: logger})

	// The bridge should have registered project 42 with the
	// adopted dir. OpenExisting must now resolve it.
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
	ws, err := workspace.NewWorkspace(tmp, logger)
	if err != nil {
		t.Fatal(err)
	}
	_ = New(Config{Workspace: ws, Logger: logger}) // no ProjectRegistry
	// Nothing was registered → OpenExisting on an arbitrary id
	// returns ErrCloneNotFound, exactly as without the bridge.
	_, err = ws.OpenExisting(99)
	if !errors.Is(err, workspace.ErrCloneNotFound) {
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
	ws, err := workspace.NewWorkspace(wsRoot, logger)
	if err != nil {
		t.Fatal(err)
	}
	_ = New(Config{Workspace: ws, ProjectRegistry: projectreg.Open(regPath), Logger: logger})

	_, err = ws.OpenExisting(77)
	if !errors.Is(err, workspace.ErrCloneNotFound) {
		t.Errorf("stale entry should be filtered; OpenExisting got: %v", err)
	}
}
