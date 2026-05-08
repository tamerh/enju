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

// openThreeBotWorkflowsForRead extends openTwoBotWorkflowsForRead
// for tests that need a third reader (e.g. reviewer-bot watching
// alice + bob produce in parallel). Same projectID, three
// distinct on-disk clones, one shared bare.
func openThreeBotWorkflowsForRead(t *testing.T) (alice, bob, carol *Workflow, bare string) {
	t.Helper()
	bare = initBareForWorkspaceTest(t)

	projectHome := t.TempDir()
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))

	for _, e := range []struct {
		name    string
		out     **Workflow
	}{
		{"alice", &alice},
		{"bob", &bob},
		{"carol", &carol},
	} {
		path := filepath.Join(projectHome, "enju", "bots", e.name, "clone")
		c, err := ws.OpenBotCloneAt(7, path, bare)
		if err != nil {
			t.Fatalf("%s clone: %v", e.name, err)
		}
		*e.out = c
	}
	return alice, bob, carol, bare
}

// TestCrossBotRead_ParallelWrites_ReaderSeesBothIntegration pins
// the parallel-bot scenario: two developer bots push different
// topic branches concurrently, a reviewer reads both. Without the
// reader-side fetch, reviewer would see one branch's content (or
// none) depending on which one happens to be in its local object
// DB. With the lazy fetch, both reads succeed.
//
// This is the multi-developer pattern (dev_a + dev_b in parallel,
// reviewer judges combined output) the parallel-merge work was
// supposed to enable end-to-end. The push side already worked;
// this test pins that the read side does too.
func TestCrossBotRead_ParallelWrites_ReaderSeesBothIntegration(t *testing.T) {
	alice, bob, carol, _ := openThreeBotWorkflowsForRead(t)

	// Alice + bob push concurrently to disjoint topic branches.
	type result struct {
		sha string
		err error
	}
	results := make([]result, 2)
	done := make(chan struct{}, 2)
	go func() {
		res, err := alice.SubmitTaskResult(SubmitRequest{
			TaskID:         "7:1:dev_a",
			BranchOverride: "topic-a",
			Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
			Files:          []FileWrite{{RepoRelPath: "out/a.md", Content: []byte("alice work\n")}},
		})
		if err != nil {
			results[0] = result{err: err}
		} else {
			results[0] = result{sha: res.CommitSHA}
		}
		done <- struct{}{}
	}()
	go func() {
		res, err := bob.SubmitTaskResult(SubmitRequest{
			TaskID:         "7:1:dev_b",
			BranchOverride: "topic-b",
			Citizen:        Identity{Name: "bob", Email: "bob@example.com"},
			Files:          []FileWrite{{RepoRelPath: "out/b.md", Content: []byte("bob work\n")}},
		})
		if err != nil {
			results[1] = result{err: err}
		} else {
			results[1] = result{sha: res.CommitSHA}
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("parallel push %d: %v", i, r.err)
		}
	}
	aliceSHA, bobSHA := results[0].sha, results[1].sha

	// Carol (reviewer-bot) reads both. First read triggers a
	// fetch that brings in BOTH topic branches; the second is a
	// pure local lookup.
	body, found, rerr := carol.ReadFileAtCommit(aliceSHA, "out/a.md")
	if rerr != nil || !found || string(body) != "alice work\n" {
		t.Errorf("carol read alice: body=%q found=%v err=%v", body, found, rerr)
	}
	body, found, rerr = carol.ReadFileAtCommit(bobSHA, "out/b.md")
	if rerr != nil || !found || string(body) != "bob work\n" {
		t.Errorf("carol read bob: body=%q found=%v err=%v", body, found, rerr)
	}
}

// TestCrossBotRead_AfterAutoMerge_ReaderSeesMainIntegration pins
// the FF auto-merge path: developer pushes a topic, auto-merges
// it onto the run branch (main), reviewer reads main from their
// own clone. This is the standard answer→review flow with two
// citizens; pre-fix reviewer's clone might not have fetched main
// since the merge.
//
// Maps project's MergeBranchToCommit to enjugit's
// AutoMergeAcceptedTopic (the higher-level Workflow merge verb).
func TestCrossBotRead_AfterAutoMerge_ReaderSeesMainIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	// Developer pushes a topic branch with content.
	res, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev",
		BranchOverride: "topic-merged",
		Citizen:        Identity{Name: "developer", Email: "d@example.com"},
		Files:          []FileWrite{{RepoRelPath: "src/feature.go", Content: []byte("// feature\n")}},
	})
	if err != nil {
		t.Fatalf("developer submit: %v", err)
	}
	devSHA := res.CommitSHA

	// Auto-merge the topic onto main (FF case — main hasn't
	// advanced since topic forked from it).
	if _, err := developer.AutoMergeAcceptedTopic("topic-merged", "main",
		MergeAuthor{
			TaskID:       "7:1:dev",
			AutoOrManual: "auto",
			Citizen:      Identity{Name: "Developer", Email: "d@example.com"},
		}); err != nil {
		t.Fatalf("auto-merge: %v", err)
	}

	// Reviewer reads main via the merged commit SHA. Reviewer's
	// clone has not seen this commit yet — the lazy fetch in
	// ReadFileAtCommit must rescue.
	body, found, rerr := reviewer.ReadFileAtCommit(devSHA, "src/feature.go")
	if rerr != nil {
		t.Fatalf("reviewer read after auto-merge: %v", rerr)
	}
	if !found || string(body) != "// feature\n" {
		t.Errorf("reviewer post-merge: body=%q found=%v", body, found)
	}
}

// TestCrossBotRead_SequentialMerges_ReaderTracksMainIntegration
// pins the review-of-iterating-developer scenario: developer
// pushes + auto-merges several iterations sequentially; reviewer
// reads each iteration's commit independently. Earlier reads must
// not "stick" the reviewer's clone to one revision — every fresh
// read sees the right SHA's content.
//
// Tests across-iterations cross-bot reads, complementing the
// single-iteration AfterAutoMerge test above. Non-FF merge-commit
// fallback isn't exercised here (covered by the merge integration
// suite); this focuses on whether the cross-bot lazy-fetch keeps
// up with multiple advances on main.
func TestCrossBotRead_SequentialMerges_ReaderTracksMainIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	type iter struct {
		topic string
		path  string
		body  string
		sha   string
	}
	iters := []iter{
		{topic: "topic-iter1", path: "v1.md", body: "iter1\n"},
		{topic: "topic-iter2", path: "v2.md", body: "iter2\n"},
		{topic: "topic-iter3", path: "v3.md", body: "iter3\n"},
	}
	for i := range iters {
		res, err := developer.SubmitTaskResult(SubmitRequest{
			TaskID:         "7:1:dev",
			BranchOverride: iters[i].topic,
			Citizen:        Identity{Name: "developer", Email: "d@x.com"},
			Files:          []FileWrite{{RepoRelPath: iters[i].path, Content: []byte(iters[i].body)}},
		})
		if err != nil {
			t.Fatalf("dev submit %s: %v", iters[i].topic, err)
		}
		iters[i].sha = res.CommitSHA
		// FF-merge each iteration onto main before the next.
		if _, err := developer.AutoMergeAcceptedTopic(iters[i].topic, "main",
			MergeAuthor{
				TaskID:       "7:1:dev",
				AutoOrManual: "auto",
				Citizen:      Identity{Name: "Developer", Email: "d@x.com"},
			}); err != nil {
			t.Fatalf("dev merge %s: %v", iters[i].topic, err)
		}
	}

	// Reviewer reads each iteration's commit. Reads are
	// independent — earlier iterations' content shouldn't shadow
	// later ones, and vice versa.
	for _, it := range iters {
		body, found, rerr := reviewer.ReadFileAtCommit(it.sha, it.path)
		if rerr != nil {
			t.Errorf("reviewer read %s: %v", it.topic, rerr)
			continue
		}
		if !found {
			t.Errorf("reviewer found nothing for %s at %s", it.path, it.topic)
			continue
		}
		if string(body) != it.body {
			t.Errorf("reviewer %s: body=%q want %q", it.topic, body, it.body)
		}
	}
}

// openOperatorAndBotWorkflows sets up a realistic operator + bot
// pair: operator uses ForProject (the same path the human's
// `enju mcp` session uses, sourced through the workspace opener),
// bot uses OpenBotCloneAt (per-bot managed clone). Both clone
// from the same shared bare. Returns separate Workspaces so the
// cache-collision behavior between the two modes mirrors
// production (different processes, different in-memory state)
// instead of the single-Workspace cases above.
func openOperatorAndBotWorkflows(t *testing.T) (operator, bot *Workflow, bare string) {
	t.Helper()
	bare = initBareForWorkspaceTest(t)

	// Operator side: uses ForProject (adopted-dir / workspace
	// path). The Workspace's rootDir hosts the operator's clone.
	opWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	var err error
	operator, err = opWS.ForProject(7, bare)
	if err != nil {
		t.Fatalf("operator ForProject: %v", err)
	}

	// Bot side: uses OpenBotCloneAt (per-bot managed clone at an
	// explicit path). Separate Workspace so both clones are fully
	// independent — that's the production split between the
	// operator's `enju mcp` process and a `enju bot run` daemon
	// process.
	botWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	botPath := filepath.Join(t.TempDir(), "enju", "bots", "developer-bot", "clone")
	bot, err = botWS.OpenBotCloneAt(7, botPath, bare)
	if err != nil {
		t.Fatalf("bot clone: %v", err)
	}
	return operator, bot, bare
}

// TestCrossBotRead_OperatorWritesBotReadsIntegration pins the
// case where the operator commits something (e.g. a manual seed
// file, a task result the human filled in directly) and a bot
// downstream needs to read it as upstream context. Bot's clone
// has never seen the commit; lazy fetch must rescue.
func TestCrossBotRead_OperatorWritesBotReadsIntegration(t *testing.T) {
	operator, bot, _ := openOperatorAndBotWorkflows(t)

	res, err := operator.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:human_seed",
		BranchOverride: "topic-human-seed",
		Citizen:        Identity{Name: "tamer", Email: "tamer@example.com"},
		Files:          []FileWrite{{RepoRelPath: "design/brief.md", Content: []byte("human-authored brief\n")}},
	})
	if err != nil {
		t.Fatalf("operator submit: %v", err)
	}

	// Bot reads at the operator's commit SHA. Bot's clone has no
	// record of this commit yet — lazy fetch fixes it.
	body, found, rerr := bot.ReadFileAtCommit(res.CommitSHA, "design/brief.md")
	if rerr != nil {
		t.Fatalf("bot read of operator commit: %v", rerr)
	}
	if !found || string(body) != "human-authored brief\n" {
		t.Errorf("bot read: body=%q found=%v", body, found)
	}
}

// TestCrossBotRead_BotWritesOperatorReadsIntegration pins the
// symmetric case: bot pushes (typical autonomous-developer flow),
// then the operator's MCP session reads the same SHA via inbox /
// run_status / iteration history. Operator's clone hasn't fetched
// since the bot's push; lazy fetch must rescue. Same shape as the
// production webui "iteration content unavailable" symptom.
func TestCrossBotRead_BotWritesOperatorReadsIntegration(t *testing.T) {
	operator, bot, _ := openOperatorAndBotWorkflows(t)

	res, err := bot.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev",
		BranchOverride: "topic-dev-iter1",
		Citizen:        Identity{Name: "developer-bot", Email: "dev@example.com"},
		Files:          []FileWrite{{RepoRelPath: "src/feature.go", Content: []byte("// developed\n")}},
	})
	if err != nil {
		t.Fatalf("bot submit: %v", err)
	}

	// Operator's MCP renders run_status / inbox / web UI by
	// reading at the bot's commit SHA. Pre-fix this is where the
	// "object not found" warning fires; post-fix lazy fetch
	// self-heals.
	body, found, rerr := operator.ReadFileAtCommit(res.CommitSHA, "src/feature.go")
	if rerr != nil {
		t.Fatalf("operator read of bot commit: %v", rerr)
	}
	if !found || string(body) != "// developed\n" {
		t.Errorf("operator read: body=%q found=%v", body, found)
	}
}

// TestCrossBotRead_OperatorAndBot_ParallelPushesEachReadsOtherIntegration
// pins the bidirectional case: operator and bot each push
// concurrently to disjoint topic branches, then each reads the
// other's commit. This is the shape you get when a human is
// working interactively while a bot daemon runs autonomously in
// the background — both citizens advance the project at the same
// time.
func TestCrossBotRead_OperatorAndBot_ParallelPushesEachReadsOtherIntegration(t *testing.T) {
	operator, bot, _ := openOperatorAndBotWorkflows(t)

	type pushResult struct {
		sha string
		err error
	}
	results := make([]pushResult, 2)
	done := make(chan struct{}, 2)
	go func() {
		res, err := operator.SubmitTaskResult(SubmitRequest{
			TaskID:         "7:1:human",
			BranchOverride: "topic-human",
			Citizen:        Identity{Name: "tamer", Email: "tamer@example.com"},
			Files:          []FileWrite{{RepoRelPath: "human.md", Content: []byte("from human\n")}},
		})
		if err != nil {
			results[0] = pushResult{err: err}
		} else {
			results[0] = pushResult{sha: res.CommitSHA}
		}
		done <- struct{}{}
	}()
	go func() {
		res, err := bot.SubmitTaskResult(SubmitRequest{
			TaskID:         "7:1:bot",
			BranchOverride: "topic-bot",
			Citizen:        Identity{Name: "developer-bot", Email: "dev@example.com"},
			Files:          []FileWrite{{RepoRelPath: "bot.md", Content: []byte("from bot\n")}},
		})
		if err != nil {
			results[1] = pushResult{err: err}
		} else {
			results[1] = pushResult{sha: res.CommitSHA}
		}
		done <- struct{}{}
	}()
	<-done
	<-done
	for i, r := range results {
		if r.err != nil {
			t.Fatalf("parallel push %d: %v", i, r.err)
		}
	}
	humanSHA, botSHA := results[0].sha, results[1].sha

	// Operator reads bot's commit.
	body, found, rerr := operator.ReadFileAtCommit(botSHA, "bot.md")
	if rerr != nil || !found || string(body) != "from bot\n" {
		t.Errorf("operator read bot: body=%q found=%v err=%v", body, found, rerr)
	}
	// Bot reads operator's commit.
	body, found, rerr = bot.ReadFileAtCommit(humanSHA, "human.md")
	if rerr != nil || !found || string(body) != "from human\n" {
		t.Errorf("bot read operator: body=%q found=%v err=%v", body, found, rerr)
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
