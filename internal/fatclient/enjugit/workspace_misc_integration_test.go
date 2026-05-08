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

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
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

	wsA, _ := NewWorkspace(sharedRoot, NewProductionConventions(), WithLogger(nullLogger()))
	wfA, err := wsA.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsA ForProject: %v", err)
	}

	wsB, _ := NewWorkspace(sharedRoot, NewProductionConventions(), WithLogger(nullLogger()))
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

// TestSlugifyProjectDirIntegration verifies that ForProject with
// a project name creates a "{slug}-{id}" directory. This is the
// human-readable layout that production uses
// (~/.enju/workspaces/battle-test-alpha-7/) so cross-machine
// rsync, manual inspection, and tab-completion stay sane.
//
// TODO(enjugit-numeric-migration): the project package's
// equivalent test ALSO covered an auto-migration path: a clone
// that already existed under <id>/ (legacy numeric form) would
// be renamed to <slug>-<id>/ on the next ForProject call that
// passed a name. enjugit doesn't auto-migrate — projectDirLocked
// returns the existing numeric dir as-is and ForProject opens
// it. Decide later whether to restore the migration or document
// numeric as a permanent alternative form (and audit registry
// behavior so an entry's LocalPath winning over scan-based
// resolution stays consistent with that decision).
func TestSlugifyProjectDirIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	wsDir := t.TempDir()
	ws, _ := NewWorkspace(wsDir, NewProductionConventions(), WithLogger(nullLogger()))

	// Passing a project name creates a slug-based dir.
	wf, err := ws.ForProject(7, bare, "Battle Test Alpha")
	if err != nil {
		t.Fatalf("clone with name: %v", err)
	}
	expected := filepath.Join(wsDir, "battle-test-alpha-7")
	if wf.WorkDir() != expected {
		t.Errorf("expected workdir %s, got %s", expected, wf.WorkDir())
	}
	if _, err := os.Stat(expected); err != nil {
		t.Errorf("expected slug-id dir on disk: %v", err)
	}

	// Cross-check via the workspace's own resolver — opening the
	// same project a second time without re-passing the name
	// must return the same dir (slug-id wins over numeric in
	// projectDirLocked's scan).
	wf2, err := ws.ForProject(7, bare)
	if err != nil {
		t.Fatalf("reopen by id: %v", err)
	}
	if wf2.WorkDir() != expected {
		t.Errorf("reopen returned %s, want %s", wf2.WorkDir(), expected)
	}
}
