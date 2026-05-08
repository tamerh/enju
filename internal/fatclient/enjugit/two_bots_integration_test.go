package enjugit

// Multi-bot parallelism contract: two bots running on the same
// project on the same machine must NOT collide. Each bot owns
// its own clone at <project>/enju/bots/<botname>/clone/, its own
// per-clone lockfile, and its own working tree. The shared bare
// push target serializes their pushes via git's own ref-update
// protocol, not via filesystem locking.
//
// Pre-fix: every bot resolved to <project>/enju/.clone/, the
// shared clone made parallel daemons unsafe (claude -p in one
// daemon corrupted the other's working tree mid-branch-switch),
// and the lockfile was a single per-project lock that
// bottlenecked any "parallel" work to serial commits.
//
// Originally lived in internal/fatclient/project as
// two_bots_test.go.

import (
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// openTwoBotWorkflows materializes per-bot clones at
// <projectHome>/enju/bots/{alice,bob}/clone/ from a shared bare
// remote. Returns the two *Workflow handles and the bare path
// so tests can verify pushes landed.
//
// Both bots open through the same Workspace (matching
// production: one fat-client process holds one Workspace and
// each bot identity calls OpenBotCloneAt).
func openTwoBotWorkflows(t *testing.T) (alice, bob *Workflow, bare string) {
	t.Helper()
	bare = initBareForWorkspaceTest(t)

	projectHome := t.TempDir()
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))

	aliceClone := filepath.Join(projectHome, "enju", "bots", "alice", "clone")
	bobClone := filepath.Join(projectHome, "enju", "bots", "bob", "clone")

	var err error
	alice, err = ws.OpenBotCloneAt(7, aliceClone, bare)
	if err != nil {
		t.Fatalf("alice clone: %v", err)
	}
	bob, err = ws.OpenBotCloneAt(7, bobClone, bare)
	if err != nil {
		t.Fatalf("bob clone: %v", err)
	}
	return alice, bob, bare
}

// TestTwoBots_DistinctClonesIntegration pins the structural
// invariant: two bots opened against the same projectID get back
// genuinely different *Workflow instances at different paths.
// Pre-fix the cache returned the same handle, so the second call
// would silently inherit the first bot's working tree.
func TestTwoBots_DistinctClonesIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflows(t)
	if alice == bob {
		t.Fatal("OpenBotCloneAt returned the same Workflow for two different bot paths")
	}
	if alice.WorkDir() == bob.WorkDir() {
		t.Errorf("clones share workDir %q", alice.WorkDir())
	}
	// Each clone's path matches the documented bots layout.
	for _, c := range []struct{ name, path string }{
		{"alice", alice.WorkDir()},
		{"bob", bob.WorkDir()},
	} {
		want := "/enju/bots/" + c.name + "/clone"
		if !strings.HasSuffix(c.path, want) {
			t.Errorf("%s clone path %q doesn't end with %q", c.name, c.path, want)
		}
	}
}

// TestTwoBots_ConcurrentPushesBothLandIntegration is the
// load-bearing parallelism test: launch both bots' submits from
// goroutines at the same time, verify both commits arrive at the
// bare. If shared state remained (single clone, single index,
// single lock), this would deadlock or corrupt under load. With
// per-bot state it's deterministically clean.
//
// Each bot writes to its own topic branch — different branches
// mean no FF contention; we want to test the clone-side
// parallelism, not git's ref-update protocol.
func TestTwoBots_ConcurrentPushesBothLandIntegration(t *testing.T) {
	alice, bob, bare := openTwoBotWorkflows(t)

	push := func(wf *Workflow, branch, taskID, file, body string) error {
		_, err := wf.SubmitTaskResult(SubmitRequest{
			TaskID:         taskID,
			BranchOverride: branch,
			Citizen:        Identity{Name: branch, Email: branch + "@example.com"},
			Files: []FileWrite{
				{RepoRelPath: file, Content: []byte(body)},
			},
		})
		return err
	}

	var wg sync.WaitGroup
	wg.Add(2)
	errs := make(chan error, 2)
	go func() {
		defer wg.Done()
		if err := push(alice, "alice-topic", "7:1:t-alice", "out/alice.md", "alice"); err != nil {
			errs <- err
		}
	}()
	go func() {
		defer wg.Done()
		if err := push(bob, "bob-topic", "7:1:t-bob", "out/bob.md", "bob"); err != nil {
			errs <- err
		}
	}()
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Errorf("concurrent push failed: %v", err)
	}

	// Verify both branches reached the bare with non-zero tips.
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("opening bare: %v", err)
	}
	for _, branch := range []string{"alice-topic", "bob-topic"} {
		ref, err := bareRepo.Reference(plumbing.NewBranchReferenceName(branch), true)
		if err != nil {
			t.Errorf("bare missing branch %q: %v", branch, err)
			continue
		}
		if ref.Hash().IsZero() {
			t.Errorf("bare branch %q has zero hash", branch)
		}
	}
}

// TestTwoBots_SeparateLockfilesIntegration pins the lock-isolation
// invariant. Each bot's flock lives next to its own clone, so two
// bots on the same project don't bottleneck on a single lock.
// The check: hold alice's lock via WithLock, then try to acquire
// bob's WithLock from a goroutine. If locks are shared, bob blocks
// indefinitely; if isolated, bob acquires in microseconds. A short
// timeout catches the shared case.
//
// Drives at the *git.Clone WithLock surface (the lock primitive
// lives on the clone object, not on Workflow). Workflow
// indirection through wf.git.(*git.Clone) is the same pattern
// used in TestCrossWorkspaceFlockSerializationIntegration.
func TestTwoBots_SeparateLockfilesIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflows(t)

	cloneA, ok := alice.git.(*git.Clone)
	if !ok {
		t.Fatalf("expected *git.Clone under alice, got %T", alice.git)
	}
	cloneB, ok := bob.git.(*git.Clone)
	if !ok {
		t.Fatalf("expected *git.Clone under bob, got %T", bob.git)
	}

	// Alice acquires + holds; signals when inside, blocks until
	// the test releases.
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
	<-aIn

	// Bob's WithLock must run while alice still holds her lock —
	// per-bot flock means the locks are independent.
	bDone := make(chan struct{})
	go func() {
		_ = cloneB.WithLock(func(_ git.Ops) error {
			close(bDone)
			return nil
		})
	}()

	select {
	case <-bDone:
		// Expected: bob acquired + released while alice still
		// held her own lock. Locks are isolated.
	case <-time.After(2 * time.Second):
		t.Fatal("bob.WithLock blocked while alice held her own lock — locks are not isolated")
	}

	// Release alice and wait for her goroutine to return cleanly.
	close(aRelease)
	if err := <-aDone; err != nil {
		t.Fatalf("alice WithLock returned error: %v", err)
	}
}
