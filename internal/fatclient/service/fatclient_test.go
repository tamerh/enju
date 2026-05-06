package service

// Tests for FatClient construction. Particularly the
// adopted-project bridge: registry → workspace.externalDirs at
// New() time, so a fatclient process restarted after `enju_init
// --path=/external/dir` can still resolve that project to its
// adopted location.

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

	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/enju-ai/enju/internal/fatclient/coord"
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

// TestResolveBotWorkspace_DistinctFromAdoptedDir pins the
// production symptom from ISSUE-007: when a project is
// registered as adopted (operator did `enju_init --path=`),
// the bot's working directory must be a SEPARATE managed
// clone, not the operator's adopted tree.
//
// Pre-fix the bot daemon called ResolveProjectWorkspace,
// which honored the externalDirs short-circuit and returned
// the operator's path. Bot branch switches then jammed on
// the operator's uncommitted changes; bot writes polluted
// the operator's git status; develop tasks left tracked-
// then-untracked residue across iterations.
//
// Post-fix the daemon calls ResolveBotWorkspace, which forces
// a managed clone at `~/.enju/workspaces/<slug>-<id>/`. The
// adopted dir is still the clone source (origin), so pull/
// push work via git's local protocol — but the working trees
// are physically separate.
func TestResolveBotWorkspace_DistinctFromAdoptedDir(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()

	// Operator's adopted tree: a real git clone at /op/tree.
	adoptedDir := filepath.Join(tmp, "op-tree")
	initRealClone(t, adoptedDir)

	// Persist the registry entry — what `enju_init --path=` does.
	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{
		ID:        42,
		LocalPath: adoptedDir,
		Name:      "demo-project",
	}); err != nil {
		t.Fatal(err)
	}

	// Coord stub returning empty remote_url (the operator's
	// project has no real remote — just the adopted local tree).
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             42,
			"name":           "demo-project",
			"remote_url":     "",
			"default_branch": "main",
		})
	}))
	defer srv.Close()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, err := workspace.NewWorkspace(wsRoot, logger)
	if err != nil {
		t.Fatal(err)
	}
	c := coord.New(coord.Config{
		BaseURL:   srv.URL,
		Username:  "tamer",
		AuthToken: "t",
		Logger:    logger,
	})
	fc := New(Config{
		Coord:           c,
		Workspace:       ws,
		Logger:          logger,
		ProjectRegistry: projectreg.Open(regPath),
	})

	got, err := fc.ResolveBotWorkspace(context.Background(), 42)
	if err != nil {
		t.Fatalf("ResolveBotWorkspace: %v", err)
	}
	// Critical assertion: bot's workspace is NOT the operator's tree.
	if got == adoptedDir {
		t.Fatalf("bot workspace must be distinct from operator's adopted dir; both got %q", got)
	}
	// And it must live under the workspace root.
	if !strings.HasPrefix(got, wsRoot) {
		t.Errorf("bot workspace should live under %q, got %q", wsRoot, got)
	}
	// And it must be a real git clone.
	if _, statErr := os.Stat(filepath.Join(got, ".git")); statErr != nil {
		t.Errorf("bot workspace %q has no .git: %v", got, statErr)
	}
}

func TestResolveBotWorkspace_NoRemoteAndNoAdoptedPath_Errors(t *testing.T) {
	// Project with no remote_url and not in the registry → bot
	// can't materialize a clone. Daemon must error rather than
	// fall back to inheriting cwd.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             99,
			"name":           "ghost",
			"remote_url":     "",
			"default_branch": "main",
		})
	}))
	defer srv.Close()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, _ := workspace.NewWorkspace(wsRoot, logger)
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "x", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, Workspace: ws, Logger: logger}) // no ProjectRegistry

	_, err := fc.ResolveBotWorkspace(context.Background(), 99)
	if err == nil {
		t.Fatal("expected error when neither remote_url nor adopted path is available")
	}
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("error should be ErrNoCloneSource so the daemon's Run loop can exit on permanent config errors; got: %v", err)
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

// initRealCloneWithAuthor is initRealClone but with an
// explicit author. The vanilla helper relies on go-git reading
// the operator's ~/.gitconfig for user.name/user.email; tests
// that t.Setenv("HOME", tmp) hide that file and need to
// provide the author themselves.
func initRealCloneWithAuthor(t *testing.T, dir string) {
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
	if _, err := wt.Commit("seed", &gogit.CommitOptions{
		All:    true,
		Author: &object.Signature{Name: "test", Email: "test@example.com", When: time.Unix(0, 0)},
	}); err != nil {
		t.Fatal(err)
	}
}

// TestResolveBotWorkspace_RejectsManagedWorkspacePathAsSource
// pins the bug from the create_project-no-path scenario:
// EagerInitProjectClone registers the workspace path itself
// (under ws.RootDir()) as the project's LocalPath. Pre-fix the
// daemon's ResolveBotWorkspace blindly used that as a clone
// source and ForceManagedClone tried to clone a path into
// itself, eventually hitting openOrClone's bootstrap-empty-
// remote path which set origin = the workspace path (self-
// reference). The bot then couldn't push anywhere sensible.
//
// Post-fix: registry entries pointing inside the workspace
// root are filtered out, ResolveBotWorkspace returns
// ErrNoCloneSource, the daemon exits cleanly with operator
// guidance.
func TestResolveBotWorkspace_RejectsManagedWorkspacePathAsSource(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	// Plant the "workspace clone" dir EagerInit would have
	// created. Doesn't need to be a valid repo for the check —
	// the filter fires before any clone op.
	managedClone := filepath.Join(wsRoot, "demo-7")
	_ = os.MkdirAll(managedClone, 0o755)

	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{ID: 7, LocalPath: managedClone, Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id":             7,
			"name":           "demo",
			"remote_url":     "",
			"default_branch": "main",
		})
	}))
	defer srv.Close()

	ws, _ := workspace.NewWorkspace(wsRoot, logger)
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, Workspace: ws, Logger: logger, ProjectRegistry: projectreg.Open(regPath)})

	_, err := fc.ResolveBotWorkspace(context.Background(), 7)
	if err == nil {
		t.Fatal("expected ErrNoCloneSource for create_project-no-path projects (registry path is the workspace clone itself)")
	}
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("expected ErrNoCloneSource so the daemon exits cleanly; got %v", err)
	}
}

// TestEnsureBotPushTarget_RejectsManagedWorkspacePathAsSource
// covers the symmetric case for `enju bot setup`: when the
// only registered LocalPath is the workspace clone, refuse to
// promote (promoting would create a bare that the bot's clone
// would still land on top of, since both bot and operator's
// clone resolve to the same ws.projectDir path). Operator must
// pick a different shape.
func TestEnsureBotPushTarget_RejectsManagedWorkspacePathAsSource(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	managedClone := filepath.Join(wsRoot, "demo-7")
	_ = os.MkdirAll(managedClone, 0o755)

	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{ID: 7, LocalPath: managedClone, Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	srv, stub := newPushTargetCoord(t, "")
	defer srv.Close()

	ws, _ := workspace.NewWorkspace(wsRoot, logger)
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, Workspace: ws, Logger: logger, ProjectRegistry: projectreg.Open(regPath)})

	_, _, err := fc.EnsureBotPushTarget(context.Background(), 7)
	if err == nil {
		t.Fatal("expected ErrNoCloneSource — workspace-internal path must not be promoted to a bare")
	}
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("expected ErrNoCloneSource, got %v", err)
	}
	if stub.putCount != 0 {
		t.Errorf("must NOT PUT to coord when no valid source exists, got %d PUTs", stub.putCount)
	}
	if _, statErr := os.Stat(filepath.Join(tmp, ".enju", "repos")); statErr == nil {
		t.Errorf("must NOT create a bare under ~/.enju/repos/ when source is invalid")
	}
}

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

// TestEnsureBotPushTarget_LocalTreePromotes pins the happy
// path: project's remote_url is empty, the operator has an
// adopted tree registered. EnsureBotPushTarget must
// (a) promote the adopted tree to a bare under
//
//	$HOME/.enju/repos/{id}.git/, (b) PUT the new bare path to
//
// the coord, (c) return created=true.
func TestEnsureBotPushTarget_LocalTreePromotes(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	// HOME override redirects $HOME so os.UserHomeDir lands the
	// bare under tmp/.enju/repos/ instead of the real home dir.
	// Pin GIT_AUTHOR_* + GIT_COMMITTER_* explicitly because the
	// override also masks the real ~/.gitconfig — without these,
	// initRealClone's wt.Commit fails with "author field is
	// required" since go-git would otherwise have read .gitconfig
	// for the user.name/user.email defaults.
	t.Setenv("HOME", tmp)
	t.Setenv("GIT_AUTHOR_NAME", "test")
	t.Setenv("GIT_AUTHOR_EMAIL", "test@example.com")
	t.Setenv("GIT_COMMITTER_NAME", "test")
	t.Setenv("GIT_COMMITTER_EMAIL", "test@example.com")

	adoptedDir := filepath.Join(tmp, "op-tree")
	initRealCloneWithAuthor(t, adoptedDir)

	regPath := filepath.Join(tmp, "projects.json")
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{ID: 42, LocalPath: adoptedDir, Name: "demo"}); err != nil {
		t.Fatal(err)
	}

	srv, stub := newPushTargetCoord(t, "")
	defer srv.Close()

	wsRoot := filepath.Join(tmp, "workspaces")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, _ := workspace.NewWorkspace(wsRoot, logger)
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, Workspace: ws, Logger: logger, ProjectRegistry: projectreg.Open(regPath)})

	bareURL, created, err := fc.EnsureBotPushTarget(context.Background(), 42)
	if err != nil {
		t.Fatalf("EnsureBotPushTarget: %v", err)
	}
	if !created {
		t.Errorf("first call should report created=true")
	}
	wantBare := filepath.Join(tmp, ".enju", "repos", "42.git")
	if bareURL != wantBare {
		t.Errorf("bareURL: got %q, want %q", bareURL, wantBare)
	}
	if _, err := os.Stat(filepath.Join(wantBare, "HEAD")); err != nil {
		t.Errorf("bare not materialized at %q: %v", wantBare, err)
	}
	if stub.putCount != 1 {
		t.Errorf("expected 1 PUT to coord, got %d", stub.putCount)
	}
	if stub.gotPutBody["remote_url"] != wantBare {
		t.Errorf("PUT remote_url: got %q, want %q", stub.gotPutBody["remote_url"], wantBare)
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
	ws, _ := workspace.NewWorkspace(wsRoot, logger)
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, Workspace: ws, Logger: logger})

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
	ws, _ := workspace.NewWorkspace(wsRoot, logger)
	c := coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger})
	fc := New(Config{Coord: c, Workspace: ws, Logger: logger}) // no projectRegistry

	_, _, err := fc.EnsureBotPushTarget(context.Background(), 42)
	if err == nil {
		t.Fatal("expected error when neither remote_url nor adopted path is available")
	}
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("error should be ErrNoCloneSource so the daemon's Run loop can detect a permanent config failure and exit; got: %v", err)
	}
}
