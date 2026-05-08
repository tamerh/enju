package enjugit

// Cross-bot read scenarios. These tests close the gap that let
// production reviewer-bots fail to see developer-bot's content:
// with per-bot clones, each bot's local object DB only contains
// what THIS bot has fetched. Without an explicit fetch, bot-B
// reading bot-A's pushed commit gets "object not found" and falls
// back to a stale worktree read.
//
// The pre-fix project test suite tested write-isolation
// (TestTwoBots_*) but never cross-bot reads. These tests pin the
// READ direction. Originally lived in internal/fatclient/project
// as cross_bot_read_test.go.
//
// PHASING (matches project file's structure 1:1, ported in 7 phases):
//   Phase 1 (this commit): Theme A — lazy/eager fetch (4 tests)
//   Phase 2: Theme B-bot   — multi-clone after merge (3 tests)
//   Phase 3: Theme B-op    — operator+bot variants (3 tests)
//   Phase 4: Theme C       — OpenView remoteURL hydration (1 test)
//   Phase 5: Theme D-fork  — reviewer iter-N fork base (4 tests)
//   Phase 6: Theme D-iter  — iter-N + dirty worktree (3 tests)
//   Phase 7: Theme D-disk  — worktree-on-disk content (4 tests)

import (
	"path/filepath"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
	"github.com/go-git/go-git/v5/plumbing"
)

// openTwoBotWorkflowsForRead is the cross-bot-read variant of
// openTwoBotWorkflows: same shared bare, two distinct on-disk
// clones via OpenBotCloneAt. Named separately so the cross-bot
// read tests can evolve without entangling the two_bots
// integration suite.
//
// This is the cross-bot pattern: alice (developer-bot) writes,
// bob (reviewer-bot) reads. Each owns a clone at its own path; a
// shared bare push target lets them share commits via git's
// transport rather than via filesystem coupling.
func openTwoBotWorkflowsForRead(t *testing.T) (alice, bob *Workflow, bare string) {
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

// hasCommit returns true if wf's underlying clone has the given
// commit SHA in its local object DB. Used to assert pre-/post-
// fetch invariants without dragging plumbing into every test.
func hasCommit(t *testing.T, wf *Workflow, sha string) bool {
	t.Helper()
	clone, ok := wf.git.(*git.Clone)
	if !ok {
		t.Fatalf("expected *git.Clone under workflow, got %T", wf.git)
	}
	_, err := clone.Repo().CommitObject(plumbing.NewHash(sha))
	return err == nil
}

// TestCrossBotRead_LazyFetchOnMissIntegration is the load-bearing
// case: alice pushes a commit to a topic branch. bob's
// ReadFileAtCommit gets called WITHOUT bob ever explicitly
// fetching. The lazy fetch in ReadFileAtCommit (commit-not-found
// → fetch → retry) must self-heal so bob still reads alice's
// content.
//
// Pre-fix this would have returned "loading commit ...: object
// not found" — the exact production warning from develop_config.
func TestCrossBotRead_LazyFetchOnMissIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflowsForRead(t)

	// Alice (developer-bot) pushes a topic branch with content.
	res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev",
		BranchOverride: "topic-feature",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{RepoRelPath: "src/feature.go", Content: []byte("package feature\n\nfunc Run() {}\n")},
		},
	})
	if err != nil {
		t.Fatalf("alice submit: %v", err)
	}
	aliceSHA := res.CommitSHA

	// Sanity: bob's clone has no record of this commit yet
	// (object DB is per-clone, no fetch has fired).
	if hasCommit(t, bob, aliceSHA) {
		t.Fatal("test setup invalid: bob's clone unexpectedly has alice's commit before any fetch")
	}

	// Bob (reviewer-bot) reads alice's commit content. Without
	// the lazy-fetch fix this fails with "object not found"; with
	// it the read self-heals via fetch-and-retry.
	body, found, rerr := bob.ReadFileAtCommit(aliceSHA, "src/feature.go")
	if rerr != nil {
		t.Fatalf("cross-bot read failed (lazy fetch should have rescued): %v", rerr)
	}
	if !found {
		t.Fatal("bob couldn't find src/feature.go at alice's commit even after fetch")
	}
	want := "package feature\n\nfunc Run() {}\n"
	if string(body) != want {
		t.Errorf("bob read content %q, want %q", body, want)
	}

	// Post-fetch: alice's commit should now be in bob's local
	// object DB, so a second read is a pure local lookup.
	if !hasCommit(t, bob, aliceSHA) {
		t.Errorf("after lazy fetch, bob's clone should have alice's commit")
	}
}

// TestCrossBotRead_EagerFetchAllRefsIntegration covers the
// daemon's pre-claim path: bob calls FetchAllRefs proactively
// before any reads, so subsequent ReadFileAtCommit calls are pure
// local lookups (no per-read network round-trip). This is the
// optimization the daemon does to keep claude-p's many reads
// cheap; the lazy-fetch is the safety net.
func TestCrossBotRead_EagerFetchAllRefsIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflowsForRead(t)

	res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev",
		BranchOverride: "topic-eager",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{RepoRelPath: "design/notes.md", Content: []byte("design v1\n")},
		},
	})
	if err != nil {
		t.Fatalf("alice submit: %v", err)
	}
	aliceSHA := res.CommitSHA

	// Bob proactively fetches before reading.
	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob.FetchAllRefs: %v", err)
	}

	// Now bob's clone has alice's commit locally — the read is
	// just an object DB lookup.
	if !hasCommit(t, bob, aliceSHA) {
		t.Fatalf("after eager fetch, bob should have alice's commit")
	}

	body, found, rerr := bob.ReadFileAtCommit(aliceSHA, "design/notes.md")
	if rerr != nil || !found || string(body) != "design v1\n" {
		t.Errorf("post-fetch read: body=%q found=%v err=%v", body, found, rerr)
	}
}

// TestCrossBotRead_LazyFetchPropagatesAllBranchesIntegration
// confirms the fetch picks up every remote branch, not just one.
// After alice pushes to two distinct topic branches, bob's lazy-
// fetch on reading the FIRST commit makes the SECOND branch's
// commit readable too without a second round-trip. Reflects the
// daemon's typical case: claude-p reads several upstream task
// commits across different topics within one iteration.
func TestCrossBotRead_LazyFetchPropagatesAllBranchesIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflowsForRead(t)

	res1, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev_a",
		BranchOverride: "topic-a",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files:          []FileWrite{{RepoRelPath: "out/a.md", Content: []byte("from a\n")}},
	})
	if err != nil {
		t.Fatalf("alice push a: %v", err)
	}
	res2, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev_b",
		BranchOverride: "topic-b",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files:          []FileWrite{{RepoRelPath: "out/b.md", Content: []byte("from b\n")}},
	})
	if err != nil {
		t.Fatalf("alice push b: %v", err)
	}

	// Bob's first read triggers the lazy fetch — which is
	// `git fetch origin` (all refs), not just the specific
	// branch. So both topic-a AND topic-b commits arrive.
	if _, _, rerr := bob.ReadFileAtCommit(res1.CommitSHA, "out/a.md"); rerr != nil {
		t.Fatalf("bob's first read (topic-a) failed: %v", rerr)
	}

	// Reading topic-b's commit should now be a pure local
	// lookup — no second fetch needed because the first one
	// already brought every ref.
	if !hasCommit(t, bob, res2.CommitSHA) {
		t.Errorf("after lazy fetch on topic-a, bob should ALSO have topic-b's commit")
	}
	body, found, rerr := bob.ReadFileAtCommit(res2.CommitSHA, "out/b.md")
	if rerr != nil || !found || string(body) != "from b\n" {
		t.Errorf("topic-b read: body=%q found=%v err=%v", body, found, rerr)
	}
}

// TestCrossBotRead_ProductionRequestChangesShapeIntegration
// reproduces the exact production failure shape: developer-bot
// pushes its iter-1 deliverable on the topic branch produced by
// the conventions, reviewer-bot reads each declared file at the
// developer's commit SHA the way claude-p does. Pre-fix this hit
// "object not found" because reviewer's clone hadn't fetched.
//
// Mirrors project's TestCrossBotRead_ProductionRequestChangesShape
// — the shape was load-bearing enough that the project package
// pinned it independently of the simpler synthetic cases above.
func TestCrossBotRead_ProductionRequestChangesShapeIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	res, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:2:develop_config",
		BranchOverride: "2-build/develop_config/iter-1",
		Citizen:        Identity{Name: "developer-bot", Email: "developer@example.com"},
		Files: []FileWrite{
			{RepoRelPath: "src/config/config.go", Content: []byte("package config\n\nvar Default = \"v1\"\n")},
			{RepoRelPath: "src/config/parse.go", Content: []byte("package config\n\nfunc Parse(s string) {}\n")},
			{RepoRelPath: "go.mod", Content: []byte("module example.com/cfg\n\ngo 1.22\n")},
		},
	})
	if err != nil {
		t.Fatalf("developer submit: %v", err)
	}
	developerSHA := res.CommitSHA

	// reviewer-bot reads each declared file the way claude-p
	// would: via ReadFileAtCommit at the developer's commit SHA.
	// This is the read pattern that hit "object not found" in
	// production.
	for _, path := range []string{
		"src/config/config.go",
		"src/config/parse.go",
		"go.mod",
	} {
		body, found, rerr := reviewer.ReadFileAtCommit(developerSHA, path)
		if rerr != nil {
			t.Errorf("reviewer read %s: %v", path, rerr)
			continue
		}
		if !found {
			t.Errorf("reviewer found nothing at %s — pre-fix production symptom", path)
			continue
		}
		if len(body) == 0 {
			t.Errorf("reviewer got empty bytes for %s", path)
		}
	}
}
