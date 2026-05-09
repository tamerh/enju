package enjugit

// Cross-bot read scenarios. These tests close the gap that let
// production reviewer-bots fail to see developer-bot's content:
// with per-bot clones, each bot's local object DB only contains
// what THIS bot has fetched. Without an explicit fetch, bot-B
// reading bot-A's pushed commit gets "object not found" and falls
// back to a stale worktree read.
//
// The earlier write-isolation tests (TestTwoBots_*) didn't cover
// cross-bot reads. These tests pin the READ direction.
//
// THEMES:
//   Theme A — lazy/eager fetch (4 tests)
//   Theme B-bot   — multi-clone after merge (3 tests)
//   Theme B-op    — operator+bot variants (3 tests)
//   Theme C       — OpenView remoteURL hydration (1 test)
//   Theme D-fork  — reviewer iter-N fork base (4 tests)
//   Theme D-iter  — iter-N + dirty worktree (3 tests)
//   Theme D-disk  — worktree-on-disk content (4 tests)

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/enjugit/internal/git"
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

// ancestryContains returns true when target is reachable from
// head via the parent chain in wf's local clone. Used to verify
// that a review-iter-N commit's ancestry threads through the
// upstream's iter-N (the load-bearing fork-from-upstream
// invariant). Goes through the Ops surface so the assertion is
// backend-agnostic.
func ancestryContains(t *testing.T, wf *Workflow, head, target string) bool {
	t.Helper()
	isAnc, err := wf.git.IsAncestor(target, head)
	if err != nil {
		// One side missing locally — treat as "no" for the test
		// invariant. Surface the underlying error in case the
		// caller's failure message wants to mention it.
		t.Logf("IsAncestor(%s, %s): %v", target, head, err)
		return false
	}
	return isAnc
}

// ancestryDump returns a short text dump of head's ancestor
// SHAs. Used in failure messages to make "the ancestry doesn't
// match" diagnosable instead of opaque. Walks via the Ops
// WalkCommitsFrom primitive — no go-git imports here.
func ancestryDump(t *testing.T, wf *Workflow, head string) string {
	t.Helper()
	var lines []string
	err := wf.git.WalkCommitsFrom(head, 10, func(sha, _ string) bool {
		lines = append(lines, sha[:8])
		return true
	})
	if err != nil {
		return fmt.Sprintf("(walk error: %v)", err)
	}
	return strings.Join(lines, " → ")
}

// hasCommit returns true if wf's underlying clone has the given
// commit SHA in its local object DB. Probes via WalkCommitsFrom
// with maxWalk=1 — it short-circuits to ErrCommitNotFound when
// the starting SHA can't be resolved, which is exactly the
// "exists?" question.
func hasCommit(t *testing.T, wf *Workflow, sha string) bool {
	t.Helper()
	err := wf.git.WalkCommitsFrom(sha, 1, func(string, string) bool { return false })
	if err != nil && !errors.Is(err, git.ErrCommitNotFound) {
		t.Logf("hasCommit(%s) probe: %v", sha, err)
	}
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
// MergeAcceptedTopic (the higher-level Workflow merge verb).
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
	if _, err := developer.MergeAcceptedTopic("topic-merged", "main",
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
		if _, err := developer.MergeAcceptedTopic(iters[i].topic, "main",
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

// TestOpenView_HydratesRemoteURLForLazyFetchIntegration pins the
// regression where webui's clone (always opened via OpenView)
// had remoteURL="" even though origin was configured on disk,
// dead-locking the lazy-fetch in ReadFileAtCommit. Production
// symptom: webui showed "(content unavailable — commit
// unreachable from this clone)" for a commit that existed in the
// bare and was reachable via origin, just not yet in the local
// object DB.
//
// In enjugit, OpenView calls git.OpenClone, which hydrates
// Clone.remoteURL from the on-disk origin remote (clone.go's
// OpenClone reads origin's URL into c.remoteURL). This test
// exercises the cross-citizen read through that path: writer
// pushes via ForProject, reader opens fresh via OpenView, lazy
// fetch must succeed.
func TestOpenView_HydratesRemoteURLForLazyFetchIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	// Writer pushes a commit to the bare (the "bot" side).
	writerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	writer, err := writerWS.ForProject(7, bare)
	if err != nil {
		t.Fatalf("writer ForProject: %v", err)
	}
	res, err := writer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:dev",
		BranchOverride: "topic-x",
		Citizen:        Identity{Name: "writer", Email: "w@example.com"},
		Files:          []FileWrite{{RepoRelPath: "out.md", Content: []byte("payload\n")}},
	})
	if err != nil {
		t.Fatalf("writer submit: %v", err)
	}
	writerSHA := res.CommitSHA

	// Reader workspace: clone first via ForProject so the on-disk
	// clone exists with origin configured. Then drop the in-memory
	// caches (workflows + views) and call OpenView — mirrors the
	// webui path where each request opens fresh against a
	// pre-existing clone.
	readerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	if _, err := readerWS.ForProject(7, bare); err != nil {
		t.Fatalf("reader initial ForProject: %v", err)
	}
	// Force OpenView to do its own work (not return cached).
	readerWS.mu.Lock()
	delete(readerWS.workflows, 7)
	delete(readerWS.views, 7)
	readerWS.mu.Unlock()

	view, err := readerWS.OpenView(7)
	if err != nil {
		t.Fatalf("OpenView: %v", err)
	}

	// Assert the underlying *git.Clone hydrated remoteURL from
	// on-disk origin. git.View doesn't expose RemoteURL, so we
	// type-assert to *git.Clone (the concrete type that satisfies
	// git.View).
	clone, ok := view.git.(*git.Clone)
	if !ok {
		t.Fatalf("expected *git.Clone under view, got %T", view.git)
	}
	if clone.RemoteURL() == "" {
		t.Fatal("OpenView did not hydrate remoteURL from on-disk origin — lazy-fetch dead")
	}

	// Cross-citizen read: writerSHA isn't in reader's local
	// object DB yet. ReadFileAtCommit should self-heal via the
	// lazy fetch (which now has a remote to fetch from).
	body, found, rerr := view.ReadFileAtCommit(writerSHA, "out.md")
	if rerr != nil {
		t.Fatalf("ReadFileAtCommit on cross-citizen commit: %v", rerr)
	}
	if !found {
		t.Fatal("expected found=true after lazy fetch; pre-fix production failure mode")
	}
	if string(body) != "payload\n" {
		t.Errorf("content mismatch: got %q", body)
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

// TestReviewIter2_ForksFromUpstreamIter2_NotSeedIntegration pins
// the reported regression: when an upstream task is rejected on
// its review's iter-1 verdict and re-submits as iter-2, the
// reviewer's own iter-2 topic branch must fork from the upstream's
// iter-2 topic — NOT from the run base / seed.
//
// Production symptom: review_domain/iter-2 contained only the
// seed commit + initial commit. The upstream developer's iter-2
// content wasn't in the review's ancestry, so reviewer-bot looked
// at an empty tree and rejected ("never delivered").
//
// Threads the full sequence through SubmitTaskResult at the
// Workflow layer:
//
//	alice iter-1:  develop_domain/iter-1   (commit D1)
//	bob iter-1:    review_domain/iter-1    forked from D1
//	alice iter-2:  develop_domain/iter-2   (commit D2)
//	bob iter-2:    review_domain/iter-2    forked from D2
//
// Verifies R2's commit ancestry contains D2. If the bug
// reproduces, R2's parent chain reaches seed without touching D2.
//
// Maps project's BaseBranch field to enjugit's RunBranch field:
// SubmitTaskResult uses RunBranch as the preferred fork base
// when the topic doesn't yet exist (producing.go:88-91).
func TestReviewIter2_ForksFromUpstreamIter2_NotSeedIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflowsForRead(t)

	// === iter-1 ===
	d1Res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "1-build/develop_domain/iter-1",
		Citizen:        Identity{Name: "alice", Email: "alice@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 1\n")}},
	})
	if err != nil {
		t.Fatalf("alice iter-1 submit: %v", err)
	}
	d1SHA := d1Res.CommitSHA

	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch before review iter-1: %v", err)
	}

	r1Res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:review_domain",
		BranchOverride: "1-build/review_domain/iter-1",
		RunBranch:      "1-build/develop_domain/iter-1", // fork base when topic doesn't yet exist
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/review.md", Content: []byte("iter-1 reject\n")}},
	})
	if err != nil {
		t.Fatalf("bob iter-1 submit: %v", err)
	}
	r1SHA := r1Res.CommitSHA

	if !ancestryContains(t, bob, r1SHA, d1SHA) {
		t.Fatalf("review iter-1: R1 (%s) ancestry does not contain D1 (%s) — fork-from-upstream broken at iter-1", r1SHA, d1SHA)
	}

	// === iter-2 ===
	d2Res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "1-build/develop_domain/iter-2",
		Citizen:        Identity{Name: "alice", Email: "alice@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 2\n// iter-2 content\n")}},
	})
	if err != nil {
		t.Fatalf("alice iter-2 submit: %v", err)
	}
	d2SHA := d2Res.CommitSHA

	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch before review iter-2: %v", err)
	}

	// THE bug-reproducing call. Pre-fix this would create
	// review_domain/iter-2 forked from seed instead of from D2.
	r2Res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:review_domain",
		BranchOverride: "1-build/review_domain/iter-2",
		RunBranch:      "1-build/develop_domain/iter-2", // upstream iter-2 as fork base
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/review.md", Content: []byte("iter-2 approve\n")}},
	})
	if err != nil {
		t.Fatalf("bob iter-2 submit: %v", err)
	}
	r2SHA := r2Res.CommitSHA

	// THE pin: R2's ancestry must include D2.
	if !ancestryContains(t, bob, r2SHA, d2SHA) {
		t.Fatalf("review iter-2 forked from seed (or D1), not D2 — production bug reproduced.\n"+
			"  R2 = %s\n  D2 = %s (expected ancestor, missing)\n  D1 = %s\n"+
			"  R2 ancestry: %s",
			r2SHA, d2SHA, d1SHA, ancestryDump(t, bob, r2SHA))
	}
}

// TestReviewIter2_ForksCorrectly_EvenWithStaleLocalRefIntegration
// stresses the same surface but pre-creates a stale local ref
// pointing at the seed for the would-be review iter-2 branch.
// This is the shape that bypasses the BaseBranch logic via the
// "branch already exists" short-circuit — the candidate root cause
// for the production reproduction. The stale-ref guard must
// validate the existing ref's ancestry against RunBranch before
// honoring it.
func TestReviewIter2_ForksCorrectly_EvenWithStaleLocalRefIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflowsForRead(t)

	// alice iter-1
	if _, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "1-build/develop_domain/iter-1",
		Citizen:        Identity{Name: "alice", Email: "alice@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("v1\n")}},
	}); err != nil {
		t.Fatalf("alice iter-1: %v", err)
	}

	// Stash bob's seed (root) commit so we can plant a stale ref.
	seedHash, _, err := bob.git.Head()
	if err != nil {
		t.Fatalf("bob head: %v", err)
	}

	// alice iter-2
	d2Res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "1-build/develop_domain/iter-2",
		Citizen:        Identity{Name: "alice", Email: "alice@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("v2\n")}},
	})
	if err != nil {
		t.Fatalf("alice iter-2: %v", err)
	}
	d2SHA := d2Res.CommitSHA

	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch: %v", err)
	}

	// The stale-ref injection: pre-create review_domain/iter-2 at
	// seed BEFORE bob's submit. This is what the production clone
	// would look like if a previous run / a fetched stale tracking
	// ref planted the same branch name. SetBranchTo writes the
	// branch ref directly via Ops — no go-git needed.
	if err := bob.git.SetBranchTo("1-build/review_domain/iter-2", seedHash); err != nil {
		t.Fatalf("planting stale ref: %v", err)
	}

	r2Res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:review_domain",
		BranchOverride: "1-build/review_domain/iter-2",
		RunBranch:      "1-build/develop_domain/iter-2",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "review.md", Content: []byte("approve\n")}},
	})
	if err != nil {
		t.Fatalf("bob iter-2 submit: %v", err)
	}

	// Verify R2's ancestry includes D2 — the stale-ref guard must
	// have detected the existing ref's seed-only ancestry and
	// reseated it on D2 before commit.
	if !ancestryContains(t, bob, r2Res.CommitSHA, d2SHA) {
		t.Fatalf("REPRO: stale local ref short-circuited fork-from-base.\n"+
			"  R2 = %s\n  D2 = %s (expected ancestor, missing)\n"+
			"  R2 ancestry: %s\n"+
			"  prepareBranchForCommit honored the stale ref instead of reseating on RunBranch.",
			r2Res.CommitSHA, d2SHA, ancestryDump(t, bob, r2Res.CommitSHA))
	}
}

// TestReviewerCheckoutUpstreamTopic_ForksFromOriginNotRunBranchIntegration
// pins the reviewer pre-handler checkout: when the daemon needs
// to materialize the upstream's pushed content into the worktree
// (so claude -p can read files), CheckoutBranchFrom with empty
// baseBranch must resolve origin/<upstream-topic> — landing the
// new local ref at the developer's actual tip — NOT at run-branch.
//
// Production symptom: reviewer-bot saw an empty src/ on disk
// because the local upstream ref pointed at run-branch's tip
// instead of the developer's commit.
func TestReviewerCheckoutUpstreamTopic_ForksFromOriginNotRunBranchIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	// Run-branch baseline: a commit unrelated to develop_a's content
	// (analog of the template-snapshot commit).
	if _, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:setup",
		BranchOverride: "iter-smoke",
		Citizen:        Identity{Name: "developer", Email: "dev@x.com"},
		Files:          []FileWrite{{RepoRelPath: "templates/seed.md", Content: []byte("template scaffold\n")}},
	}); err != nil {
		t.Fatalf("setup commit: %v", err)
	}

	// Developer pushes the topic carrying the file the reviewer
	// needs to evaluate.
	devRes, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_a",
		BranchOverride: "iter-smoke-runs/develop_a/iter-1",
		RunBranch:      "iter-smoke",
		Citizen:        Identity{Name: "developer", Email: "dev@x.com"},
		Files:          []FileWrite{{RepoRelPath: "smoke/a.md", Content: []byte("magic word: enju\n")}},
	})
	if err != nil {
		t.Fatalf("develop_a iter-1 submit: %v", err)
	}
	devTopicSHA := devRes.CommitSHA

	if err := reviewer.FetchAllRefs(); err != nil {
		t.Fatalf("reviewer fetch: %v", err)
	}

	// Daemon passes EMPTY baseBranch for the review's pre-handler
	// upstream checkout. CheckoutBranchFrom resolves
	// origin/<upstream-topic>, landing the new local ref at the
	// developer's actual tip.
	if err := reviewer.CheckoutBranchFrom(
		"iter-smoke-runs/develop_a/iter-1", // target = upstream topic
		"",                                  // empty: track origin/<upstream>
	); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}

	// Verify: reviewer's local ref must point at the developer's
	// actual commit, NOT at run-branch's tip. LocalBranchHash
	// reads refs/heads/<branch> via Ops.
	gotSHA, err := reviewer.git.LocalBranchHash("iter-smoke-runs/develop_a/iter-1")
	if err != nil {
		t.Fatalf("upstream ref lookup: %v", err)
	}
	if gotSHA != devTopicSHA {
		t.Fatalf("REPRO: reviewer's local upstream ref points at the WRONG commit.\n"+
			"  got:  %s (run-branch tip — has no smoke/a.md)\n"+
			"  want: %s (developer's actual topic commit)",
			gotSHA, devTopicSHA)
	}

	// Verify: smoke/a.md is on disk (the actual symptom).
	smokePath := filepath.Join(reviewer.WorkDir(), "smoke", "a.md")
	if _, err := os.Stat(smokePath); err != nil {
		t.Errorf("smoke/a.md missing from reviewer worktree: %v", err)
	}
}

// TestReviewerIterN_ForksFromRunBranchNotUpstreamIntegration is
// the reporter's loop-forever bug nailed at the right call site.
// For iter > 1 of a REVIEW task, the daemon calls
// CheckoutBranchFrom(review_a/iter-N, upstream-topic) — passing
// upstream-topic as baseBranch so the new review-iter-N forks
// from the upstream's content (which carries the developer's
// files claude -p must evaluate), NOT from run-branch (which
// has no upstream content).
//
// Pre-fix the daemon passed run-branch as baseBranch; review iter-N
// forked from run-branch tip; worktree had no smoke/a.md; claude -p
// rejected forever in a loop.
func TestReviewerIterN_ForksFromRunBranchNotUpstreamIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	// Run-branch baseline (analog of template snapshot).
	if _, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:setup",
		BranchOverride: "iter-smoke",
		Citizen:        Identity{Name: "developer", Email: "dev@x.com"},
		Files:          []FileWrite{{RepoRelPath: "templates/seed.md", Content: []byte("scaffold\n")}},
	}); err != nil {
		t.Fatalf("setup: %v", err)
	}

	// Developer pushes upstream topic with smoke/a.md.
	if _, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_a",
		BranchOverride: "iter-smoke-runs/develop_a/iter-1",
		RunBranch:      "iter-smoke",
		Citizen:        Identity{Name: "developer", Email: "dev@x.com"},
		Files:          []FileWrite{{RepoRelPath: "smoke/a.md", Content: []byte("magic word: enju\n")}},
	}); err != nil {
		t.Fatalf("develop_a iter-1 submit: %v", err)
	}

	if err := reviewer.FetchAllRefs(); err != nil {
		t.Fatalf("reviewer fetch: %v", err)
	}

	// Post-fix: daemon passes UpstreamIterationBranch (not run-branch)
	// as baseBranch when the action is review. review_a/iter-2 forks
	// from upstream-topic which carries the developer's content.
	if err := reviewer.CheckoutBranchFrom(
		"iter-smoke-runs/review_a/iter-2",  // target = reviewer's own iter-N
		"iter-smoke-runs/develop_a/iter-1", // baseBranch = upstream's topic
	); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}

	smokePath := filepath.Join(reviewer.WorkDir(), "smoke", "a.md")
	if _, err := os.Stat(smokePath); err != nil {
		t.Fatalf("REPRO: review iter-N forked from run-branch (no smoke/a.md).\n"+
			"  os.Stat smoke/a.md: %v\n"+
			"This is the loop-forever bug: review iter-2+ creates a topic\n"+
			"branch forked from run-branch instead of upstream-topic, so\n"+
			"the developer's commit content isn't on disk; claude -p reads\n"+
			"empty → request_changes → iter_seq bumps → repeat.", err)
	}
}

// TestStaleRefReset_PreservesClaudeOutputIntegration pins the
// load-bearing contract: when validate-stale-ref reseats a stale
// local topic ref to RunBranch's tip, any handler output the
// caller passed in `req.Files` must NOT be lost. The daemon-side
// flow is:
//
//  1. handler (claude -p) writes src/foo.go to the worktree
//  2. daemon scans worktree, reads file content into req.Files
//  3. daemon calls SubmitTaskResult with req.Files populated
//  4. SubmitTaskResult.prepareBranchForCommit fires validate-stale-ref
//     (stale local topic at seed → reseats to RunBranch tip)
//  5. SubmitTaskResult writes req.Files back from the in-memory
//     copies it received
//  6. commit + push
//
// If req.Files faithfully carries claude's output, the commit
// must contain it regardless of any worktree wipe during step 4.
func TestStaleRefReset_PreservesClaudeOutputIntegration(t *testing.T) {
	alice, bob, _ := openTwoBotWorkflowsForRead(t)

	// Alice publishes the run-branch tip we'll fork from.
	if _, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:setup",
		BranchOverride: "run-base",
		Citizen:        Identity{Name: "alice", Email: "alice@x.com"},
		Files:          []FileWrite{{RepoRelPath: "README.md", Content: []byte("# project\n")}},
	}); err != nil {
		t.Fatalf("alice setup: %v", err)
	}

	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch: %v", err)
	}

	// Plant the stale-ref shape: develop_config/iter-2 already
	// exists locally pointing at seed (the production bug shape).
	seedHash, _, _ := bob.git.Head()
	if err := bob.git.SetBranchTo("run-1/develop_config/iter-2", seedHash); err != nil {
		t.Fatalf("planting stale ref: %v", err)
	}

	// Simulate "claude -p just wrote files to the worktree" —
	// these are untracked files in bob's working tree at this
	// moment. The daemon would scan them and hand them to
	// SubmitTaskResult as req.Files. We mimic that exactly: write
	// files to disk, then build the Files slice from in-memory
	// copies (matching the daemon's `ReadFile + Files = ...` shape).
	claudeOutput := []FileWrite{
		{RepoRelPath: "src/config/config.go", Content: []byte("package config\n\nvar Default = \"v1\"\n")},
		{RepoRelPath: "src/go.mod", Content: []byte("module example.com/cfg\n\ngo 1.22\n")},
		{RepoRelPath: "result.md", Content: []byte("Implemented config package.\n")},
	}
	for _, f := range claudeOutput {
		full := filepath.Join(bob.WorkDir(), f.RepoRelPath)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, f.Content, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// Bob submits. The validate-stale-ref step MUST fire (ref points
	// at seed, not in run-base's ancestry) and reseat the local ref
	// to run-base's tip BEFORE the commit lands.
	res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_config",
		BranchOverride: "run-1/develop_config/iter-2",
		RunBranch:      "run-base",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          claudeOutput,
	})
	if err != nil {
		t.Fatalf("bob submit: %v", err)
	}

	// Verify the commit's tree has all of claude's output. The
	// reseat must NOT have wiped req.Files mid-flight. ReadFileAtCommit
	// reaches the tree through the Ops surface.
	for _, f := range claudeOutput {
		body, found, err := bob.git.ReadFileAtCommit(res.CommitSHA, f.RepoRelPath)
		if err != nil {
			t.Errorf("read %s at %s: %v — stale-ref reseat corrupted submit pipeline", f.RepoRelPath, res.CommitSHA, err)
			continue
		}
		if !found {
			t.Errorf("commit tree missing %s — stale-ref reseat corrupted submit pipeline", f.RepoRelPath)
			continue
		}
		if string(body) != string(f.Content) {
			t.Errorf("content mismatch for %s: got %q, want %q", f.RepoRelPath, string(body), string(f.Content))
		}
	}
}

// TestIterN_NewBranchForksFromRunBranchNotSeedIntegration pins the
// production fork-base bug. When the daemon detects iter > 1 and
// the topic branch has bumped (e.g. develop_domain/iter-2 after
// review verdict triggered iter_seq increment), it calls
// CheckoutBranchFrom(topic, run-branch) so the new topic forks
// from the run-branch's TIP (which has prior task content), NOT
// from seed.
//
// Production symptom: develop_domain/iter-2 forked from seed
// instead of build-1 tip; iter-2 was orphaned from prior task
// content; switching between worktrees of build-1 vs iter-2
// failed with "worktree contains unstaged changes" because the
// trees diverged wildly.
func TestIterN_NewBranchForksFromRunBranchNotSeedIntegration(t *testing.T) {
	_, bob, _ := openTwoBotWorkflowsForRead(t)

	// Build the run branch with two commits — seed + a run-branch
	// advance (analog of develop_config landing on build-1 before
	// develop_domain runs).
	if _, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:setup",
		BranchOverride: "build-mainline",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "README.md", Content: []byte("# project\n")}},
	}); err != nil {
		t.Fatalf("setup commit: %v", err)
	}
	advanceRes, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_config",
		BranchOverride: "build-mainline",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/config/config.go", Content: []byte("package config\n")}},
	})
	if err != nil {
		t.Fatalf("run-branch advance: %v", err)
	}
	runBranchTip := advanceRes.CommitSHA

	// iter-1 of develop_domain — committed on its topic, forked
	// from build-mainline (which now contains src/config/).
	if _, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "topics/develop_domain/iter-1",
		RunBranch:      "build-mainline",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 1\n")}},
	}); err != nil {
		t.Fatalf("iter-1 submit: %v", err)
	}

	// Simulate the daemon's iter > 1 branch step. With the fix,
	// the daemon now passes meta.Branch (run branch = build-mainline)
	// as baseBranch so the new topic forks from the run-branch tip
	// — which carries prior task content.
	if err := bob.CheckoutBranchFrom("topics/develop_domain/iter-2", "build-mainline"); err != nil {
		t.Fatalf("CheckoutBranchFrom for iter-2: %v", err)
	}

	// Verify iter-2's branch ref forks from the RUN BRANCH TIP,
	// not from seed. This is the core production failure.
	got, err := bob.git.LocalBranchHash("topics/develop_domain/iter-2")
	if err != nil {
		t.Fatalf("iter-2 ref lookup: %v", err)
	}
	if got == "" {
		t.Fatalf("iter-2 ref missing locally")
	}
	if got != runBranchTip {
		t.Fatalf("REPRO: iter-2 forked from wrong base.\n"+
			"  iter-2 commit: %s\n"+
			"  run-branch tip: %s (expected — has prior task content)\n"+
			"  This matches the production failure where iter-2/iter-3 forked\n"+
			"  from seed (origin/main) instead of build-1 tip, leaving them\n"+
			"  orphaned from prior task content.",
			got, runBranchTip)
	}
}

// TestIter2_DirtyWorktreeFromIter1_DoesNotBlockBranchSwitchIntegration
// pins the iteration-boundary bug: the daemon calls
// CheckoutBranchFrom for iter-2 BEFORE running ResetCleanWorktree.
// The bot's worktree still carries iter-1's tree + any untracked /
// modified residue from claude -p. The Force-checkout + preserve
// dance must handle that dirty state — without it, the production
// failure was "git refuses: worktree contains unstaged changes."
func TestIter2_DirtyWorktreeFromIter1_DoesNotBlockBranchSwitchIntegration(t *testing.T) {
	_, bob, _ := openTwoBotWorkflowsForRead(t)

	// Bob commits iter-1 on its own topic branch.
	if _, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "1-build/develop_domain/iter-1",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 1\n")}},
	}); err != nil {
		t.Fatalf("iter-1 submit: %v", err)
	}

	// Simulate claude -p iter-1's residue: an untracked scratch
	// file AND a modification to the just-committed tracked file.
	// Both are common shapes — claude often writes scratch notes
	// it never declares, and sometimes patches tracked files
	// outside writes_artifacts after the commit landed.
	if err := os.WriteFile(
		filepath.Join(bob.WorkDir(), "scratch.notes"),
		[]byte("untracked claude scratch\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bob.WorkDir(), "src/domain/foo.go"),
		[]byte("package domain\n\nvar V = 1\n// post-commit edit by claude\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	// Now the iter-2 invalidate-bump case: cascade marked iter-1
	// rejected, daemon re-claims with iter_seq=2 and a fresh branch
	// name. This is the call the daemon makes BEFORE any reset.
	if err := bob.CheckoutBranchFrom("1-build/develop_domain/iter-2", ""); err != nil {
		t.Fatalf("REPRO: CheckoutBranchFrom for iter-2 failed on dirty iter-1 worktree: %v\n"+
			"This is the iteration-boundary bug — the daemon calls this BEFORE\n"+
			"resetting the clone, so iter-1's residue blocks the switch.", err)
	}

	// Verify HEAD landed on iter-2 (not stuck on iter-1). Ops.Head
	// returns the short branch name (no refs/heads/ prefix).
	_, branch, err := bob.git.Head()
	if err != nil {
		t.Fatalf("HEAD: %v", err)
	}
	if got, want := branch, "1-build/develop_domain/iter-2"; got != want {
		t.Errorf("HEAD on wrong ref after checkout: got %s, want %s", got, want)
	}
}

// TestReviewerWorktreeHasUpstreamContent_BeforeHandlerRunsIntegration
// pins the daemon's pre-handler checkout: before claude -p runs on
// a review task, the upstream developer's source files MUST be
// visible on disk at the worktree root. The upstream commit's tree
// being reachable via origin/<topic> is not enough — claude -p
// reads files from the filesystem, not from git refs.
//
// Production symptom: developer pushes src/config/config.go;
// reviewer-bot fetches and the remote-tracking ref appears, but
// `ls src/config/` returns "No such file or directory" because no
// checkout fired. claude -p reports "no src/ directory exists" and
// rejects forever.
func TestReviewerWorktreeHasUpstreamContent_BeforeHandlerRunsIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	// Developer pushes a topic branch carrying real source files.
	if _, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_config",
		BranchOverride: "1-build/develop_config/iter-1",
		Citizen:        Identity{Name: "developer", Email: "d@x.com"},
		Files: []FileWrite{
			{RepoRelPath: "src/config/config.go", Content: []byte("package config\n\ntype Config struct{ Port int }\n")},
			{RepoRelPath: "src/go.mod", Content: []byte("module example.com/cfg\n")},
		},
	}); err != nil {
		t.Fatalf("developer submit: %v", err)
	}

	if err := reviewer.FetchAllRefs(); err != nil {
		t.Fatalf("reviewer fetch: %v", err)
	}

	// Sanity: the ref exists in reviewer's clone (object DB).
	// LocalBranchHash falls back to refs/remotes/origin/<branch>
	// when the local ref isn't present, so an empty return means
	// neither path resolved — fetch didn't bring the ref in.
	if h, err := reviewer.git.LocalBranchHash("1-build/develop_config/iter-1"); err != nil || h == "" {
		t.Fatalf("reviewer fetch didn't bring upstream topic ref: hash=%q err=%v", h, err)
	}

	// THE FIX: checkout the upstream topic branch tip so its content
	// materializes in the worktree before claude -p runs.
	if err := reviewer.CheckoutBranchFrom("1-build/develop_config/iter-1", ""); err != nil {
		t.Fatalf("reviewer checkout upstream topic: %v", err)
	}

	// Post-checkout: the upstream's source files are now on disk.
	srcPath := filepath.Join(reviewer.WorkDir(), "src", "config", "config.go")
	body, err := os.ReadFile(srcPath)
	if err != nil {
		t.Fatalf("after checkout, src/config/config.go still not visible: %v\n"+
			"This is the production bug — claude -p reads from disk, refs alone aren't enough.", err)
	}
	if !strings.Contains(string(body), "type Config struct") {
		t.Errorf("on-disk content doesn't match upstream commit: got %q", body)
	}
	gomod, err := os.ReadFile(filepath.Join(reviewer.WorkDir(), "src", "go.mod"))
	if err != nil {
		t.Errorf("src/go.mod missing on disk after checkout: %v", err)
	} else if !strings.Contains(string(gomod), "module example.com/cfg") {
		t.Errorf("go.mod content mismatch: got %q", gomod)
	}
}

// TestReviewerWorktreeMissingContent_LocalRefMatchesOriginIntegration
// pins the persistent reporter scenario: the local upstream ref
// already EXISTS and points at the CORRECT commit (matches origin).
// HEAD is on a different branch (the run branch). When
// CheckoutBranchFrom switches HEAD to the upstream topic, the
// worktree must be updated to match — otherwise claude -p reads
// the previous branch's tree from disk and rejects forever.
//
// Without a Force-shaped checkout, go-git's non-Force wt.Checkout
// could leave the worktree on the previous branch's tree even
// after HEAD moves. enjugit's CheckoutBranchFrom uses Force +
// preserve dance, which materializes the new branch's tree.
func TestReviewerWorktreeMissingContent_LocalRefMatchesOriginIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	// Developer pushes content.
	devRes, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_a",
		BranchOverride: "iter-smoke-runs/develop_a/iter-1",
		Citizen:        Identity{Name: "developer", Email: "d@x.com"},
		Files:          []FileWrite{{RepoRelPath: "smoke/a.md", Content: []byte("magic word: enju\n")}},
	})
	if err != nil {
		t.Fatalf("developer submit: %v", err)
	}
	devSHA := devRes.CommitSHA

	if err := reviewer.FetchAllRefs(); err != nil {
		t.Fatalf("reviewer fetch: %v", err)
	}

	// Establish the production state precisely: local upstream ref
	// EXISTS and matches origin's tip. (This is what a previous
	// successful review iteration of the same upstream would have
	// left behind.) SetBranchTo writes refs/heads/<branch> via Ops.
	upstreamBranch := "iter-smoke-runs/develop_a/iter-1"
	if err := reviewer.git.SetBranchTo(upstreamBranch, devSHA); err != nil {
		t.Fatalf("planting matching local ref: %v", err)
	}

	// Sanity: HEAD must be on a DIFFERENT branch (not the upstream
	// topic) so we exercise the worktree-update path.
	_, headBranch, _ := reviewer.git.Head()
	if headBranch == upstreamBranch {
		t.Fatal("test setup invalid: HEAD already on upstream topic; can't exercise the switch")
	}

	// Production call.
	if err := reviewer.CheckoutBranchFrom("iter-smoke-runs/develop_a/iter-1", ""); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}

	// THE PRODUCTION SYMPTOM. After the checkout, smoke/a.md must
	// be on disk. If the checkout failed to materialize the new
	// branch's tree, this test fails exactly as production did.
	smokePath := filepath.Join(reviewer.WorkDir(), "smoke", "a.md")
	body, err := os.ReadFile(smokePath)
	if err != nil {
		t.Fatalf("REPRO: smoke/a.md missing from worktree.\n"+
			"  os.ReadFile: %v\n"+
			"  HEAD post-checkout: %s\n"+
			"non-Force wt.Checkout did not write the new branch's tree to disk.\n"+
			"claude -p reads empty worktree → request_changes loop.",
			err, headRefName(t, reviewer))
	}
	if string(body) != "magic word: enju\n" {
		t.Errorf("content mismatch: got %q", body)
	}
}

// TestReviewerWorktreeMissingContent_DespiteCorrectRefIntegration
// pins the new reporter scenario after the prior fixes landed:
// branch ref appears correct on inspection (R2 forks from D2),
// but the worktree doesn't have the upstream's tree on disk.
// Hypothesized cause: a stale local refs/heads/<upstream-topic>
// from a prior bot invocation pointing at the wrong commit
// (run-branch tip instead of developer's actual tip). The new
// checkout call must reseat the local ref AND materialize the
// correct tree on disk.
func TestReviewerWorktreeMissingContent_DespiteCorrectRefIntegration(t *testing.T) {
	developer, reviewer, _ := openTwoBotWorkflowsForRead(t)

	// Developer pushes a topic branch carrying smoke/a.md.
	devRes, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_a",
		BranchOverride: "iter-smoke-runs/develop_a/iter-1",
		Citizen:        Identity{Name: "developer", Email: "d@x.com"},
		Files:          []FileWrite{{RepoRelPath: "smoke/a.md", Content: []byte("hello from a\n")}},
	})
	if err != nil {
		t.Fatalf("developer submit: %v", err)
	}
	devActualSHA := devRes.CommitSHA

	if err := reviewer.FetchAllRefs(); err != nil {
		t.Fatalf("reviewer fetch: %v", err)
	}

	// Plant the stale-ref shape: the local refs/heads/<upstream>
	// exists from a prior bot invocation pointing at the WRONG
	// commit (e.g. seed/run-branch tip — what the broken version
	// of the fix produced). origin/<upstream> still points at the
	// developer's actual commit.
	staleHash, _, _ := reviewer.git.Head()
	if staleHash == devActualSHA {
		t.Fatal("test setup invalid: HEAD coincides with developer's commit")
	}
	upstreamBranch := "iter-smoke-runs/develop_a/iter-1"
	if err := reviewer.git.SetBranchTo(upstreamBranch, staleHash); err != nil {
		t.Fatalf("planting stale ref: %v", err)
	}

	// Production flow: daemon calls CheckoutBranchFrom(upstream, "")
	// — empty baseBranch so the verb resolves origin/<upstream> as
	// the authoritative tip and reseats the local ref.
	if err := reviewer.CheckoutBranchFrom(upstreamBranch, ""); err != nil {
		t.Fatalf("CheckoutBranchFrom: %v", err)
	}

	// Verify the local ref now points at the developer's actual
	// commit (not the stale hash).
	gotSHA, err := reviewer.git.LocalBranchHash(upstreamBranch)
	if err != nil {
		t.Fatalf("ref lookup post-checkout: %v", err)
	}
	if gotSHA != devActualSHA {
		t.Errorf("REPRO: local upstream ref still stale.\n"+
			"  got:  %s (the planted stale hash)\n"+
			"  want: %s (origin's actual upstream tip)",
			gotSHA, devActualSHA)
	}

	// THE PRODUCTION SYMPTOM: smoke/a.md must be on disk after the
	// checkout. If it isn't, claude -p reads empty src/ and rejects
	// forever.
	smokePath := filepath.Join(reviewer.WorkDir(), "smoke", "a.md")
	body, err := os.ReadFile(smokePath)
	if err != nil {
		t.Fatalf("REPRO: smoke/a.md missing from reviewer worktree post-checkout: %v\n"+
			"This is the reporter's bug: branch ref appears correct but the\n"+
			"worktree on disk doesn't reflect the upstream's tree. claude -p\n"+
			"reads disk → no file → request_changes forever.", err)
	}
	if string(body) != "hello from a\n" {
		t.Errorf("smoke/a.md content mismatch: got %q", body)
	}
}

// TestRequestChanges_RevisionOnSameBranch_DirtyWorktreeIntegration
// pins the request_changes flow at iter-1: after a reviewer-bot
// asks for changes, the developer re-claims the SAME iteration
// (iter_seq stays at 1, topic branch name unchanged). claude -p
// writes revised content to the worktree (often modifying tracked
// files). Submit then calls SubmitTaskResult with the same branch
// name — the existing-ref short-circuit must not fail on unstaged
// modifications. Pre-fix the non-Force wt.Checkout returned
// ErrUnstagedChanges; with the Force-shaped path, the revision
// commit lands cleanly.
func TestRequestChanges_RevisionOnSameBranch_DirtyWorktreeIntegration(t *testing.T) {
	_, bob, _ := openTwoBotWorkflowsForRead(t)

	// Establish the run branch (analog of the operator's initial
	// create_run + first commit). Iter-1's topic forks from this.
	if _, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:setup",
		BranchOverride: "mainline",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "README.md", Content: []byte("# project\n")}},
	}); err != nil {
		t.Fatalf("setup commit: %v", err)
	}

	// Bob submits iter-1 on its topic branch (forked from mainline).
	if _, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "topics/develop_domain/iter-1",
		RunBranch:      "mainline",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 1\n")}},
	}); err != nil {
		t.Fatalf("iter-1 submit: %v", err)
	}

	// CRITICAL: simulate the pre-claim pull moving HEAD off the
	// topic branch and back onto the run branch (so the early-
	// return-when-already-on-branch short-circuit doesn't fire on
	// the subsequent submit).
	if err := bob.CheckoutBranchFrom("mainline", ""); err != nil {
		t.Fatalf("simulating pre-claim pull (move HEAD to run branch): %v", err)
	}

	// Now claude -p iter-2 writes a file. From run-branch's POV
	// this file is NEW (untracked). From iter-1's POV the path
	// already exists tracked. When CheckoutBranchFrom switches
	// from run-branch to iter-1 it must overwrite this untracked
	// file with iter-1's tracked version, then SubmitTaskResult
	// writes the revised content from req.Files.
	if err := os.MkdirAll(filepath.Join(bob.WorkDir(), "src/domain"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(bob.WorkDir(), "src/domain/foo.go"),
		[]byte("package domain\n\nvar V = 2 // revised per reviewer\n"),
		0o644,
	); err != nil {
		t.Fatal(err)
	}

	// iter-2 submits to the SAME branch (request_changes flow,
	// iter_seq still = 1). SubmitTaskResult internally calls into
	// prepareBranchForCommit which checks out the topic branch.
	if _, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:         "7:1:develop_domain",
		BranchOverride: "topics/develop_domain/iter-1", // SAME branch — revision
		RunBranch:      "mainline",
		Citizen:        Identity{Name: "bob", Email: "bob@x.com"},
		Files:          []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 2 // revised per reviewer\n")}},
	}); err != nil {
		t.Fatalf("REPRO: revision submit on same branch failed with dirty worktree: %v\n"+
			"This is the request_changes-flow bug — non-Force checkout fails on\n"+
			"unstaged modifications.", err)
	}
}

// headRefName returns the wf's current HEAD ref name as a string,
// or "(error)" / "(detached)". Helper for diagnostic failure
// messages — keeps the test bodies focused. Goes through Ops.
func headRefName(t *testing.T, wf *Workflow) string {
	t.Helper()
	_, branch, err := wf.git.Head()
	if err != nil {
		return "(error)"
	}
	if branch == "" {
		return "(detached)"
	}
	return "refs/heads/" + branch
}
