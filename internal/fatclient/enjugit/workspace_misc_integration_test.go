package enjugit

// Real-bare integration tests for two miscellaneous workspace
// behaviors: cross-workspace flock serialization and slug-id
// directory naming. Both reach into the on-disk side of
// enjugit.Workspace + git.Clone in ways the fake-ops unit
// tests can't.

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
	"github.com/enju-ai/enju/internal/fatclient/projectreg"
)

// TestCrossWorkspaceFlockSerializationIntegration verifies that
// two Workspace instances pointed at the same root dir (simulating
// two MCP processes running against the same ~/.enju/workspaces)
// serialize their git operations via the on-disk flock that
// git.Clone holds per project. The second WithLock call must
// block until the first one's callback returns.
//
// Critical for multi-process safety: an MCP server + a `enju ui`
// process can both touch the same project clone, and without the
// flock they'd race on .git/index.lock.
func TestCrossWorkspaceFlockSerializationIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	sharedRoot := t.TempDir()

	// Both workspaces share the SAME registry file + project path so
	// ForProject resolves to one on-disk clone — that's the whole
	// point of the flock contract (two processes, same .git, one
	// lock). The registry is the single source of truth for path
	// resolution post-NDW.2.
	regPath := filepath.Join(t.TempDir(), "projects.json")
	projectPath := filepath.Join(sharedRoot, "p")
	if err := os.MkdirAll(projectPath, 0o755); err != nil {
		t.Fatal(err)
	}
	regA := projectreg.Open(regPath)
	if err := regA.Upsert(projectreg.Entry{ID: 80, LocalPath: projectPath}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}

	wsA, err := NewWorkspace(sharedRoot, NewProductionConventions(),
		WithLogger(nullLogger()), WithRegistry(regA))
	if err != nil {
		t.Fatal(err)
	}
	wfA, err := wsA.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsA ForProject: %v", err)
	}

	regB := projectreg.Open(regPath)
	wsB, err := NewWorkspace(sharedRoot, NewProductionConventions(),
		WithLogger(nullLogger()), WithRegistry(regB))
	if err != nil {
		t.Fatal(err)
	}
	wfB, err := wsB.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsB ForProject: %v", err)
	}

	// Sanity: different in-process handles (each workspace has
	// its own workflows map) but pointing at the same clone on
	// disk. The flock binds to the path, not the in-process
	// object identity.
	if wfA == wfB {
		t.Fatal("expected distinct Workflow instances across Workspaces")
	}
	if wfA.WorkDir() != wfB.WorkDir() {
		t.Fatalf("expected same work dir across Workspaces, got %q vs %q",
			wfA.WorkDir(), wfB.WorkDir())
	}

	cloneA, ok := wfA.git.(*git.Clone)
	if !ok {
		t.Fatalf("expected *git.Clone under wfA, got %T", wfA.git)
	}
	cloneB, ok := wfB.git.(*git.Clone)
	if !ok {
		t.Fatalf("expected *git.Clone under wfB, got %T", wfB.git)
	}

	// A holds the lock via WithLock and signals that it's in.
	// B starts WithLock in another goroutine; expected to block
	// until A releases. Two channels: aIn (A is inside its
	// callback), aRelease (test tells A's callback to return).
	aIn := make(chan struct{})
	aRelease := make(chan struct{})
	aDone := make(chan error, 1)
	go func() {
		aDone <- cloneA.WithLock(func(_ git.Ops) error {
			close(aIn)
			<-aRelease
			return nil
		})
	}()
	<-aIn // wait until A is holding the lock

	// B tries to acquire — should block until A's callback returns.
	bDone := make(chan struct{})
	go func() {
		_ = cloneB.WithLock(func(_ git.Ops) error {
			close(bDone)
			return nil
		})
	}()

	select {
	case <-bDone:
		t.Fatal("wfB.WithLock callback ran while wfA was still holding the lock")
	case <-time.After(50 * time.Millisecond):
		// Expected: B is blocked on A.
	}

	// Release A and verify B proceeds.
	close(aRelease)
	if err := <-aDone; err != nil {
		t.Fatalf("wfA WithLock returned: %v", err)
	}
	select {
	case <-bDone:
		// Good: B acquired once A released.
	case <-time.After(2 * time.Second):
		t.Fatal("wfB.WithLock never ran after wfA released the lock")
	}
}

// TestForProjectResolvesViaRegistry pins the post-NDW.2 path
// resolution: ForProject ignores the projectName argument and
// opens the clone at the registry's LocalPath, regardless of how
// the caller spelled the project name. The slug-id layout is no
// longer the workspace's responsibility — operators chose paths
// via enju_create_project path=<abs/dir>.
func TestForProjectResolvesViaRegistry(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	wsDir := t.TempDir()
	regPath := filepath.Join(t.TempDir(), "projects.json")
	chosenPath := filepath.Join(wsDir, "operators-chosen-name")
	if err := os.MkdirAll(chosenPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := projectreg.Open(regPath)
	if err := reg.Upsert(projectreg.Entry{ID: 7, LocalPath: chosenPath}); err != nil {
		t.Fatalf("registry upsert: %v", err)
	}
	ws, err := NewWorkspace(wsDir, NewProductionConventions(),
		WithLogger(nullLogger()), WithRegistry(reg))
	if err != nil {
		t.Fatal(err)
	}

	// Passing a different name does NOT change the resolved path
	// — the registry wins. (projectName is accepted for back-compat
	// with NDW.1 callers and ignored; NDW.5 removes the parameter.)
	wf, err := ws.ForProject(7, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if wf.WorkDir() != chosenPath {
		t.Errorf("expected workdir %s, got %s", chosenPath, wf.WorkDir())
	}
}
