package service

// Tests for FatClient construction. Particularly the
// adopted-project bridge: registry → project.externalDirs at
// New() time, so a fatclient process restarted after
// `enju_create_project path=/external/dir` can still resolve
// that project to its adopted location.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/coord"
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

// pushTargetCoordStub stands in for the coord during
// EnsureBotPushTarget tests. It serves GET /projects/{id} with a
// configurable remote_url, and accepts the PUT
// /projects/{id}/remote that the helper issues after promoting
// the operator's tree to a bare. The PUT body is captured so
// tests can assert what was sent.
type pushTargetCoordStub struct {
	remoteURL  string
	gotPutBody map[string]string
	putCount   int
}

func newPushTargetCoord(t *testing.T, initialRemote string) (*httptest.Server, *pushTargetCoordStub) {
	t.Helper()
	s := &pushTargetCoordStub{remoteURL: initialRemote}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/projects/"):
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id":             42,
				"name":           "demo",
				"remote_url":     s.remoteURL,
				"default_branch": "main",
			})
		case r.Method == http.MethodPut && strings.HasSuffix(r.URL.Path, "/remote"):
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &s.gotPutBody)
			s.putCount++
			s.remoteURL = s.gotPutBody["remote_url"]
			w.WriteHeader(http.StatusNoContent)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	return srv, s
}

// TestEnsureBotPushTarget_LocalTreePromotes pins the happy path:
// project's remote_url is empty, registry has the project's
// home path. EnsureBotPushTarget must
//
//	(a) promote the home tree to a bare INSIDE the project at
//	    `<home>/enju/.bare.git/`,
//	(b) NOT PUT to the coord (the bare is local-per-machine),
//	(c) return created=true.
//
// Idempotency: a second call sees the existing bare and returns
// created=false without re-cloning.
func TestEnsureBotPushTarget_LocalTreePromotes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	// Pin GIT_AUTHOR_* + GIT_COMMITTER_* — initRealCloneWithAuthor
	// supplies them via an explicit signature, but go-git's
	// CommitOptions.All path still consults the env in some go-git
	// versions. Keeps the test deterministic across versions.
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	homeTree := filepath.Join(tmp, "op-tree")
	initRealCloneWithAuthor(t, homeTree)

	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{ID: 42, LocalPath: homeTree, Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	srv, stub := newPushTargetCoord(t, "")
	defer srv.Close()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, _ := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, WorkspaceRoot:   ws.RootDir(), Logger: logger, ProjectRegistry: projectreg.Open(regPath)})

	bareURL, created, err := fc.EnsureBotPushTarget(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureBotPushTarget: %v", err)
	}
	if !created {
		t.Errorf("first call should report created=true")
	}
	wantBare := filepath.Join(homeTree, "enju", ".bare.git")
	if bareURL != wantBare {
		t.Errorf("bareURL: got %q, want %q", bareURL, wantBare)
	}
	if _, err := os.Stat(filepath.Join(wantBare, "HEAD")); err != nil {
		t.Errorf("bare not materialized at %q: %v", wantBare, err)
	}
	if stub.putCount != 0 {
		t.Errorf("must NOT PUT to coord — bare is purely local; got %d PUTs", stub.putCount)
	}

	// Second call must be idempotent — no re-clone, created=false.
	_, created2, err := fc.EnsureBotPushTarget(context.Background(), 42)
	if err != nil {
		t.Fatalf("second EnsureBotPushTarget: %v", err)
	}
	if created2 {
		t.Errorf("second call should report created=false")
	}
}

// TestEnsureBotPushTarget_RealRemoteIsNoOp confirms that when
// the project already has a real (https/git/ssh) remote — i.e.
// github plays the bare role — EnsureBotPushTarget short-
// circuits without writing anything to disk or calling PUT.
func TestEnsureBotPushTarget_RealRemoteIsNoOp(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv, stub := newPushTargetCoord(t, "https://github.com/example/demo.git")
	defer srv.Close()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, _ := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, WorkspaceRoot:   ws.RootDir(), Logger: logger})

	url, created, err := fc.EnsureBotPushTarget(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureBotPushTarget: %v", err)
	}
	if created {
		t.Errorf("real-remote project should not promote (created=false)")
	}
	if url != "https://github.com/example/demo.git" {
		t.Errorf("expected existing remote URL returned unchanged, got %q", url)
	}
	if stub.putCount != 0 {
		t.Errorf("real-remote project should not PUT to coord, got %d PUTs", stub.putCount)
	}
	if _, err := os.Stat(filepath.Join(tmp, ".enju", "repos")); err == nil {
		t.Errorf("real-remote project should not create a bare under ~/.enju/repos/")
	}
}

// TestEnsureBotPushTarget_NoSourceErrors covers the failure
// case the operator hits when they ran `enju bot setup` against
// a project that was created without an adopted path AND
// without a remote_url. Helper must error out with a clear
// hint, NOT silently no-op.
func TestEnsureBotPushTarget_NoSourceErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	srv, _ := newPushTargetCoord(t, "")
	defer srv.Close()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, _ := enjugit.NewWorkspace(wsRoot, enjugit.NewProductionConventions(), enjugit.WithLogger(logger))
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, WorkspaceRoot:   ws.RootDir(), Logger: logger}) // no projectRegistry

	_, _, err := fc.EnsureBotPushTarget(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error when neither remote_url nor adopted path is available")
	}
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("error should be ErrNoCloneSource so the daemon's Run loop can detect a permanent config failure and exit; got: %v", err)
	}
}
