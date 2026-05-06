package project

// Multi-bot parallelism contract: two bots running on the
// same project on the same machine must NOT collide. Each
// bot owns its own clone at <project>/enju/bots/<botname>/clone/,
// its own per-clone lockfile, and its own working tree. The
// shared bare push target serializes their pushes via git's
// own ref-update protocol, not via filesystem locking.
//
// Pre-fix: every bot resolved to <project>/enju/.clone/, the
// shared clone made parallel daemons unsafe (claude -p in
// one daemon would corrupt the other's working tree mid-
// branch-switch), and the lockfile was a single per-project
// `<workspace>/project-{id}.lock` that bottlenecked any
// "parallel" work to serial commits.
//
// These tests pin the post-fix invariants by exercising both
// bot clones from the same Opener, sometimes concurrently.

import (
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// openTwoBotClones materializes per-bot clones at
// <projectHome>/enju/bots/{alice,bob}/clone/ from a shared
// bare remote. Returns the two *Clone handles and the bare
// path so tests can verify pushes landed.
func openTwoBotClones(t *testing.T) (alice, bob *Clone, bare string) {
	t.Helper()
	bare = initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	projectHome := t.TempDir()
	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}

	aliceClone := filepath.Join(projectHome, "enju", "bots", "alice", "clone")
	bobClone := filepath.Join(projectHome, "enju", "bots", "bob", "clone")

	// Same projectID — both bots are members of the same project.
	// The cache must NOT collapse them onto a single entry.
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

// TestTwoBots_DistinctClones pins the structural invariant:
// two bots opened against the same projectID get back
// genuinely different *Clone instances at different paths.
// Pre-fix the Opener cache returned the same handle, so the
// second call would silently inherit the first bot's working
// tree.
func TestTwoBots_DistinctClones(t *testing.T) {
	alice, bob, _ := openTwoBotClones(t)
	if alice == bob {
		t.Fatal("OpenBotCloneAt returned the same Clone for two different bot paths")
	}
	if alice.WorkDir() == bob.WorkDir() {
		t.Errorf("clones share workDir %q", alice.WorkDir())
	}
	// Each clone's path matches the documented bots layout.
	for _, c := range []struct {
		name, path string
	}{
		{"alice", alice.WorkDir()},
		{"bob", bob.WorkDir()},
	} {
		want := "/enju/bots/" + c.name + "/clone"
		if got := c.path; len(got) < len(want) || got[len(got)-len(want):] != want {
			t.Errorf("%s clone path %q doesn't end with %q", c.name, got, want)
		}
	}
}

// TestTwoBots_ConcurrentPushesBothLand is the load-bearing
// parallelism test: launch both bots' pushes from goroutines
// at the same time, verify both commits arrive at the bare.
// If shared state remained (single clone, single index,
// single lock), this would deadlock or corrupt under load —
// possibly flakily, but at least sometimes. With per-bot
// state it's deterministically clean.
func TestTwoBots_ConcurrentPushesBothLand(t *testing.T) {
	alice, bob, bare := openTwoBotClones(t)

	// Each bot writes to its own topic branch — different
	// branches mean no FF contention; we want to test the
	// clone-side parallelism, not git's ref-update protocol.
	push := func(c *Clone, branch, taskID, file, body string) error {
		c.Lock()
		defer c.Unlock()
		_, err := c.SubmitTaskResult(SubmitRequest{
			TaskID:   taskID,
			Username: branch, // doubles as commit author for visibility
			Branch:   branch,
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

	// Verify both branches reached the bare with the right tip.
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

// TestTwoBots_SeparateLockfiles pins the lock-isolation
// invariant. Each bot's flock lives next to its own clone,
// so two bots on the same project don't bottleneck on a
// single lock. The check: hold alice's lock, then try to
// acquire bob's from a goroutine. If locks are shared, bob
// blocks indefinitely; if they're isolated, bob acquires in
// microseconds. A short timeout catches the shared case.
func TestTwoBots_SeparateLockfiles(t *testing.T) {
	alice, bob, _ := openTwoBotClones(t)
	alice.Lock()
	defer alice.Unlock()

	done := make(chan struct{})
	go func() {
		bob.Lock()
		bob.Unlock()
		close(done)
	}()
	select {
	case <-done:
		// bob acquired + released while alice held its own
		// lock — separate locks confirmed.
	case <-time.After(2 * time.Second):
		t.Fatal("bob.Lock() blocked while alice held its own lock — locks are not isolated")
	}
}
