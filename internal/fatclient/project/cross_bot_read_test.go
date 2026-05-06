package project

// Cross-bot read scenarios. These tests close the gap that let
// the production reviewer-bot fail to see developer-bot's
// content: with per-bot clones, each bot's local object DB
// only contains what THIS bot has fetched. Without an explicit
// fetch, bot-B reading bot-A's pushed commit gets "object not
// found" and falls back to a stale worktree read.
//
// The pre-fix project test suite tested write-isolation
// (TestTwoBots_*) but never cross-bot reads. These tests pin
// the read direction.

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/go-git/go-git/v5/plumbing"
)

// TestCrossBotRead_LazyFetchOnMiss is the load-bearing case:
// alice pushes a commit to a topic branch. bob's ReadFileAtCommit
// gets called WITHOUT bob ever explicitly fetching. The lazy
// fetch in ReadFileAtCommit (commit-not-found → fetch → retry)
// must self-heal so bob still reads alice's content.
//
// Pre-fix this would have returned "loading commit ...: object
// not found" — the exact production warning from develop_config.
func TestCrossBotRead_LazyFetchOnMiss(t *testing.T) {
	alice, bob, _ := openTwoBotClones(t)

	// Alice (developer-bot) pushes a topic branch with content.
	alice.Lock()
	res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev",
		Username: "alice",
		Branch:   "topic-feature",
		Files: []FileWrite{
			{RepoRelPath: "src/feature.go", Content: []byte("package feature\n\nfunc Run() {}\n")},
		},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice submit: %v", err)
	}
	aliceSHA := res.CommitSHA

	// Sanity: bob's clone has no record of this commit yet
	// (object DB is per-clone).
	if _, lookupErr := bob.repo.CommitObject(plumbing.NewHash(aliceSHA)); lookupErr == nil {
		t.Fatal("test setup invalid: bob's clone unexpectedly has alice's commit before any fetch")
	}

	// Bob (reviewer-bot) reads alice's commit content. Without
	// the lazy-fetch fix this fails with "object not found";
	// with the fix the read self-heals via fetch-and-retry.
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
	if _, lookupErr := bob.repo.CommitObject(plumbing.NewHash(aliceSHA)); lookupErr != nil {
		t.Errorf("after lazy fetch, bob's clone should have alice's commit; got %v", lookupErr)
	}
}

// TestCrossBotRead_EagerFetchAllRefs covers the daemon's pre-
// claim path: bob calls FetchAllRefs proactively before any
// reads, so subsequent ReadFileAtCommit calls are pure local
// lookups (no per-read network round-trip). This is the
// optimization the daemon does to keep claude-p's many reads
// cheap; the lazy-fetch in ReadFileAtCommit is the safety net.
func TestCrossBotRead_EagerFetchAllRefs(t *testing.T) {
	alice, bob, _ := openTwoBotClones(t)

	// Alice pushes.
	alice.Lock()
	res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev",
		Username: "alice",
		Branch:   "topic-eager",
		Files: []FileWrite{
			{RepoRelPath: "design/notes.md", Content: []byte("design v1\n")},
		},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice submit: %v", err)
	}
	aliceSHA := res.CommitSHA

	// Bob proactively fetches before reading.
	bob.Lock()
	fetchErr := bob.FetchAllRefs()
	bob.Unlock()
	if fetchErr != nil {
		t.Fatalf("bob.FetchAllRefs: %v", fetchErr)
	}

	// Now bob's clone has alice's commit locally — the read is
	// just an object DB lookup.
	if _, lookupErr := bob.repo.CommitObject(plumbing.NewHash(aliceSHA)); lookupErr != nil {
		t.Fatalf("after eager fetch, bob should have alice's commit; got %v", lookupErr)
	}

	body, found, rerr := bob.ReadFileAtCommit(aliceSHA, "design/notes.md")
	if rerr != nil || !found || string(body) != "design v1\n" {
		t.Errorf("post-fetch read: body=%q found=%v err=%v", body, found, rerr)
	}
}

// TestCrossBotRead_LazyFetchPropagatesAllBranches confirms the
// fetch picks up every remote branch, not just one. After alice
// pushes to two distinct topic branches, bob's lazy-fetch on
// reading the FIRST commit makes the SECOND branch's commit
// readable too without a second round-trip. Reflects the
// daemon's typical case: claude-p reads several upstream task
// commits across different topics within one iteration.
func TestCrossBotRead_LazyFetchPropagatesAllBranches(t *testing.T) {
	alice, bob, _ := openTwoBotClones(t)

	// Alice pushes two topic branches.
	alice.Lock()
	res1, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev_a",
		Username: "alice",
		Branch:   "topic-a",
		Files:    []FileWrite{{RepoRelPath: "out/a.md", Content: []byte("from a\n")}},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice push a: %v", err)
	}
	alice.Lock()
	res2, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev_b",
		Username: "alice",
		Branch:   "topic-b",
		Files:    []FileWrite{{RepoRelPath: "out/b.md", Content: []byte("from b\n")}},
	})
	alice.Unlock()
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
	if _, hasIt := bob.repo.CommitObject(plumbing.NewHash(res2.CommitSHA)); hasIt != nil {
		t.Errorf("after lazy fetch on topic-a, bob should ALSO have topic-b's commit; got %v", hasIt)
	}
	body, found, rerr := bob.ReadFileAtCommit(res2.CommitSHA, "out/b.md")
	if rerr != nil || !found || string(body) != "from b\n" {
		t.Errorf("topic-b read: body=%q found=%v err=%v", body, found, rerr)
	}
}

// openThreeBotClones extends openTwoBotClones for tests that
// need a third reader (e.g. reviewer-bot watching alice + bob
// produce in parallel). Same projectID, three distinct on-disk
// clones, one shared bare.
func openThreeBotClones(t *testing.T) (alice, bob, carol *Clone, bare string) {
	t.Helper()
	bare = initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	projectHome := t.TempDir()
	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("Opener: %v", err)
	}
	for _, e := range []struct {
		name  string
		cloneOut **Clone
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
		*e.cloneOut = c
	}
	return alice, bob, carol, bare
}

// TestCrossBotRead_ParallelWrites_ReaderSeesBoth pins the
// parallel-bot scenario: two developer bots push different
// topic branches concurrently, a reviewer reads both. Without
// the reader-side fetch, reviewer would see one branch's
// content (or none) depending on which one happens to be in
// its local object DB. With the lazy fetch, both reads
// succeed.
//
// This is the multi-developer pattern (dev_a + dev_b in
// parallel, reviewer judges combined output) the parallel-
// merge work was supposed to enable end-to-end. The push side
// already worked; this test pins that the read side does too.
func TestCrossBotRead_ParallelWrites_ReaderSeesBoth(t *testing.T) {
	alice, bob, carol, _ := openThreeBotClones(t)

	// Alice + bob push concurrently to disjoint topic branches.
	var wg sync.WaitGroup
	wg.Add(2)
	results := make([]string, 2)
	errs := make([]error, 2)
	go func() {
		defer wg.Done()
		alice.Lock()
		res, err := alice.SubmitTaskResult(SubmitRequest{
			TaskID:   "7:1:dev_a",
			Username: "alice",
			Branch:   "topic-a",
			Files:    []FileWrite{{RepoRelPath: "out/a.md", Content: []byte("alice work\n")}},
		})
		alice.Unlock()
		if err != nil {
			errs[0] = err
			return
		}
		results[0] = res.CommitSHA
	}()
	go func() {
		defer wg.Done()
		bob.Lock()
		res, err := bob.SubmitTaskResult(SubmitRequest{
			TaskID:   "7:1:dev_b",
			Username: "bob",
			Branch:   "topic-b",
			Files:    []FileWrite{{RepoRelPath: "out/b.md", Content: []byte("bob work\n")}},
		})
		bob.Unlock()
		if err != nil {
			errs[1] = err
			return
		}
		results[1] = res.CommitSHA
	}()
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("parallel push %d: %v", i, err)
		}
	}
	aliceSHA, bobSHA := results[0], results[1]

	// Carol (reviewer-bot) reads both. First read triggers a
	// fetch that brings in BOTH topic branches; the second is
	// a pure local lookup.
	body, found, rerr := carol.ReadFileAtCommit(aliceSHA, "out/a.md")
	if rerr != nil || !found || string(body) != "alice work\n" {
		t.Errorf("carol read alice: body=%q found=%v err=%v", body, found, rerr)
	}
	body, found, rerr = carol.ReadFileAtCommit(bobSHA, "out/b.md")
	if rerr != nil || !found || string(body) != "bob work\n" {
		t.Errorf("carol read bob: body=%q found=%v err=%v", body, found, rerr)
	}
}

// TestCrossBotRead_AfterAutoMerge_ReaderSeesMain pins the FF
// auto-merge path: developer pushes a topic, auto-merges it
// onto the run branch (`main`), reviewer reads main from their
// own clone. This is the standard answer→review flow with two
// citizens; pre-fix reviewer's clone might not have fetched
// main since the merge.
func TestCrossBotRead_AfterAutoMerge_ReaderSeesMain(t *testing.T) {
	developer, reviewer, _ := openTwoBotClones(t)

	// Developer pushes a topic branch with content.
	developer.Lock()
	res, err := developer.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev",
		Username: "developer",
		Branch:   "topic-merged",
		Files:    []FileWrite{{RepoRelPath: "src/feature.go", Content: []byte("// feature\n")}},
	})
	if err != nil {
		developer.Unlock()
		t.Fatalf("developer submit: %v", err)
	}
	devSHA := res.CommitSHA

	// Auto-merge the topic onto main (FF case).
	if err := developer.MergeBranchToCommit("main", devSHA, "topic-merged",
		MergeAuthor{Name: "Developer", Email: "d@example.com", TaskID: "7:1:dev"}); err != nil {
		developer.Unlock()
		t.Fatalf("auto-merge: %v", err)
	}
	developer.Unlock()

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

// TestCrossBotRead_SequentialMerges_ReaderTracksMain pins the
// review-of-iterating-developer scenario: developer pushes +
// auto-merges several iterations sequentially; reviewer reads
// each iteration's commit independently. Earlier reads must
// not "stick" the reviewer's clone to one revision — every
// fresh read sees the right SHA's content.
//
// Tests across-iterations cross-bot reads, complementing the
// single-iteration AfterAutoMerge test above. Non-FF merge-
// commit fallback isn't exercised here (it's covered by
// TestMergeBranchToCommit_NonFFDisjointWrites within one
// clone); this focuses on whether the cross-bot lazy-fetch
// keeps up with multiple advances on main.
func TestCrossBotRead_SequentialMerges_ReaderTracksMain(t *testing.T) {
	developer, reviewer, _ := openTwoBotClones(t)

	// Three sequential iterations, each FF-merged to main.
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
		developer.Lock()
		res, err := developer.SubmitTaskResult(SubmitRequest{
			TaskID:   "7:1:dev",
			Username: "developer",
			Branch:   iters[i].topic,
			Files:    []FileWrite{{RepoRelPath: iters[i].path, Content: []byte(iters[i].body)}},
		})
		if err != nil {
			developer.Unlock()
			t.Fatalf("dev submit %s: %v", iters[i].topic, err)
		}
		iters[i].sha = res.CommitSHA
		// FF-merge each iteration onto main before the next.
		if err := developer.MergeBranchToCommit("main", res.CommitSHA, iters[i].topic,
			MergeAuthor{Name: "Developer", Email: "d@x.com", TaskID: "7:1:dev"}); err != nil {
			developer.Unlock()
			t.Fatalf("dev merge %s: %v", iters[i].topic, err)
		}
		developer.Unlock()
	}

	// Reviewer reads each iteration's commit. Reads are
	// independent — earlier iterations' content shouldn't
	// shadow later ones, and vice versa.
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

// openOperatorAndBotClones sets up a realistic operator + bot
// pair: operator uses ForProject (the same path the human's
// `enju mcp` session uses, sourced through the workspace
// opener), bot uses OpenBotCloneAt (per-bot managed clone).
// Both clone from the same shared bare. Returns separate
// Openers so the cache-collision behavior between the two
// modes mirrors production (different processes, different
// in-memory state) instead of the single-Opener test cases
// above.
func openOperatorAndBotClones(t *testing.T) (operator, bot *Clone, bare string) {
	t.Helper()
	bare = initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Operator side: uses ForProject (adopted-dir / workspace
	// path). The Opener's rootDir hosts the operator's clone.
	opWS, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("operator Opener: %v", err)
	}
	operator, err = opWS.ForProject(7, bare)
	if err != nil {
		t.Fatalf("operator clone: %v", err)
	}

	// Bot side: uses OpenBotCloneAt (per-bot managed clone at
	// an explicit path). Separate Opener so both clones are
	// fully independent — that's the production split between
	// the operator's `enju mcp` process and a `enju bot run`
	// daemon process.
	botWS, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("bot Opener: %v", err)
	}
	botPath := filepath.Join(t.TempDir(), "enju", "bots", "developer-bot", "clone")
	bot, err = botWS.OpenBotCloneAt(7, botPath, bare)
	if err != nil {
		t.Fatalf("bot clone: %v", err)
	}
	return operator, bot, bare
}

// TestCrossBotRead_OperatorWritesBotReads pins the case where
// the operator commits something (e.g. a manual seed file, a
// task result the human filled in directly) and a bot
// downstream needs to read it as upstream context. Bot's clone
// has never seen the commit; lazy fetch must rescue.
func TestCrossBotRead_OperatorWritesBotReads(t *testing.T) {
	operator, bot, _ := openOperatorAndBotClones(t)

	// Operator pushes content via the standard submit path.
	operator.Lock()
	res, err := operator.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:human_seed",
		Username: "tamer",
		Branch:   "topic-human-seed",
		Files:    []FileWrite{{RepoRelPath: "design/brief.md", Content: []byte("human-authored brief\n")}},
	})
	operator.Unlock()
	if err != nil {
		t.Fatalf("operator submit: %v", err)
	}

	// Bot reads at the operator's commit SHA. Bot's clone has
	// no record of this commit yet — lazy fetch fixes it.
	body, found, rerr := bot.ReadFileAtCommit(res.CommitSHA, "design/brief.md")
	if rerr != nil {
		t.Fatalf("bot read of operator commit: %v", rerr)
	}
	if !found || string(body) != "human-authored brief\n" {
		t.Errorf("bot read: body=%q found=%v", body, found)
	}
}

// TestCrossBotRead_BotWritesOperatorReads pins the symmetric
// case: bot pushes (typical autonomous-developer flow), then
// the operator's MCP session reads the same SHA via inbox /
// run_status / iteration history. Operator's clone hasn't
// fetched since the bot's push; lazy fetch must rescue. This
// is the same shape as the production webui "iteration
// content unavailable" symptom.
func TestCrossBotRead_BotWritesOperatorReads(t *testing.T) {
	operator, bot, _ := openOperatorAndBotClones(t)

	// Bot pushes its iter-1 deliverable.
	bot.Lock()
	res, err := bot.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev",
		Username: "developer-bot",
		Branch:   "topic-dev-iter1",
		Files:    []FileWrite{{RepoRelPath: "src/feature.go", Content: []byte("// developed\n")}},
	})
	bot.Unlock()
	if err != nil {
		t.Fatalf("bot submit: %v", err)
	}

	// Operator's MCP renders run_status / inbox / web UI by
	// reading at the bot's commit SHA. Pre-fix this is where
	// the "object not found" warning fires; post-fix lazy
	// fetch self-heals.
	body, found, rerr := operator.ReadFileAtCommit(res.CommitSHA, "src/feature.go")
	if rerr != nil {
		t.Fatalf("operator read of bot commit: %v", rerr)
	}
	if !found || string(body) != "// developed\n" {
		t.Errorf("operator read: body=%q found=%v", body, found)
	}
}

// TestCrossBotRead_OperatorAndBot_ParallelPushesEachReadsOther
// pins the bidirectional case: operator and bot each push
// concurrently to disjoint topic branches, then each reads
// the other's commit. This is the shape you get when a human
// is working interactively while a bot daemon runs autonomously
// in the background — both citizens advance the project at
// the same time.
func TestCrossBotRead_OperatorAndBot_ParallelPushesEachReadsOther(t *testing.T) {
	operator, bot, _ := openOperatorAndBotClones(t)

	var wg sync.WaitGroup
	wg.Add(2)
	type pushResult struct {
		sha string
		err error
	}
	results := make([]pushResult, 2)
	go func() {
		defer wg.Done()
		operator.Lock()
		res, err := operator.SubmitTaskResult(SubmitRequest{
			TaskID:   "7:1:human",
			Username: "tamer",
			Branch:   "topic-human",
			Files:    []FileWrite{{RepoRelPath: "human.md", Content: []byte("from human\n")}},
		})
		operator.Unlock()
		if err != nil {
			results[0] = pushResult{err: err}
			return
		}
		results[0] = pushResult{sha: res.CommitSHA}
	}()
	go func() {
		defer wg.Done()
		bot.Lock()
		res, err := bot.SubmitTaskResult(SubmitRequest{
			TaskID:   "7:1:bot",
			Username: "developer-bot",
			Branch:   "topic-bot",
			Files:    []FileWrite{{RepoRelPath: "bot.md", Content: []byte("from bot\n")}},
		})
		bot.Unlock()
		if err != nil {
			results[1] = pushResult{err: err}
			return
		}
		results[1] = pushResult{sha: res.CommitSHA}
	}()
	wg.Wait()
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

// TestCrossBotRead_ProductionRequestChangesShape simulates the
// exact production scenario: developer-bot writes to a topic
// branch, reviewer-bot reads to assess. Pre-fix reviewer-bot
// saw an empty branch and emitted bogus request_changes. With
// the fix reviewer-bot reads the actual content.
//
// This is the cross-bot read scenario the test suite was
// missing before the production rejection loop surfaced it.
func TestCrossBotRead_ProductionRequestChangesShape(t *testing.T) {
	developerClone, reviewerClone, _ := openTwoBotClones(t)

	// developer-bot writes its iter-1 deliverable.
	developerClone.Lock()
	res, err := developerClone.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:2:develop_config",
		Username: "developer-bot",
		Branch:   "2-build/develop_config/iter-1",
		Files: []FileWrite{
			{RepoRelPath: "src/config/config.go", Content: []byte("package config\n\nvar Default = \"v1\"\n")},
			{RepoRelPath: "src/config/parse.go", Content: []byte("package config\n\nfunc Parse(s string) {}\n")},
			{RepoRelPath: "go.mod", Content: []byte("module example.com/cfg\n\ngo 1.22\n")},
		},
	})
	developerClone.Unlock()
	if err != nil {
		t.Fatalf("developer submit: %v", err)
	}
	developerSHA := res.CommitSHA

	// reviewer-bot reads each declared file the way claude-p
	// would: via ReadFileAtCommit at the developer's commit
	// SHA. This is the read pattern that was hitting "object
	// not found" in production.
	for _, path := range []string{
		"src/config/config.go",
		"src/config/parse.go",
		"go.mod",
	} {
		body, found, rerr := reviewerClone.ReadFileAtCommit(developerSHA, path)
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

// TestOpenExisting_HydratesRemoteURLForLazyFetch pins the regression
// where webui's clone (always opened via OpenExisting) had remoteURL=""
// even though origin was configured on disk, dead-locking the lazy-
// fetch in ReadFileAtCommit. Production symptom: webui showed
// "(content unavailable — commit unreachable from this clone)" for a
// commit that existed in the bare and was reachable via origin, just
// not yet in the local object DB.
//
// The fix populates Clone.remoteURL from the on-disk origin
// remote during OpenExisting; this test exercises the cross-
// citizen read through that path.
func TestOpenExisting_HydratesRemoteURLForLazyFetch(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Writer pushes a commit to the bare (the "bot" side).
	writerWS, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("writer Opener: %v", err)
	}
	writer, err := writerWS.ForProject(7, bare)
	if err != nil {
		t.Fatalf("writer ForProject: %v", err)
	}
	writer.Lock()
	res, err := writer.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:dev",
		Username: "writer",
		Branch:   "topic-x",
		Files:    []FileWrite{{RepoRelPath: "out.md", Content: []byte("payload\n")}},
	})
	writer.Unlock()
	if err != nil {
		t.Fatalf("writer submit: %v", err)
	}
	writerSHA := res.CommitSHA

	// Reader workspace: clone first via ForProject so the on-
	// disk clone exists with origin configured. Then drop the
	// in-memory cache and re-open via OpenExisting — mirrors
	// the webui path where each request opens fresh against a
	// pre-existing clone.
	readerWS, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("reader Opener: %v", err)
	}
	if _, err := readerWS.ForProject(7, bare); err != nil {
		t.Fatalf("reader initial ForProject: %v", err)
	}
	// Force OpenExisting to do its own work (not return cached).
	readerWS.mu.Lock()
	delete(readerWS.clients, 7)
	readerWS.mu.Unlock()

	reader, err := readerWS.OpenExisting(7)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	if reader.remoteURL == "" {
		t.Fatal("OpenExisting did not hydrate remoteURL from on-disk origin — lazy-fetch dead")
	}

	// Cross-citizen read: writerSHA isn't in reader's local
	// object DB yet. ReadFileAtCommit should self-heal via the
	// lazy fetch (which now has a remote to fetch from).
	body, found, rerr := reader.ReadFileAtCommit(writerSHA, "out.md")
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

// TestReviewIter2_ForksFromUpstreamIter2_NotSeed pins the
// reported regression: when an upstream task is rejected on its
// review's iter-1 verdict and re-submits as iter-2, the
// reviewer's own iter-2 topic branch must fork from the
// upstream's iter-2 topic — NOT from the run base / seed.
//
// Production symptom: review_domain/iter-2 contained only the
// seed commit + initial commit. The upstream developer's iter-2
// content wasn't in the review's ancestry, so reviewer-bot
// looked at an empty tree and rejected ("never delivered").
//
// This test threads the full sequence through SubmitTaskResult
// at the project layer:
//
//   alice iter-1:  develop_domain/iter-1   (commit D1)
//   bob iter-1:    review_domain/iter-1    forked from D1 (R1's parent chain → D1)
//   alice iter-2:  develop_domain/iter-2   (commit D2 — independent topic)
//   bob iter-2:    review_domain/iter-2    forked from D2 (R2's parent chain → D2)
//
// Verifies that R2's commit ancestry contains D2. If the bug
// reproduces here, R2's parent chain reaches seed without
// touching D2.
func TestReviewIter2_ForksFromUpstreamIter2_NotSeed(t *testing.T) {
	alice, bob, bare := openTwoBotClones(t)
	_ = bare

	// === iter-1 ===
	alice.Lock()
	d1Res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:develop_domain",
		Username: "alice",
		Branch:   "1-build/develop_domain/iter-1",
		Files:    []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 1\n")}},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice iter-1 submit: %v", err)
	}
	d1SHA := d1Res.CommitSHA

	// Bob fetches so he can fork from alice's topic.
	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch before review iter-1: %v", err)
	}

	bob.Lock()
	r1Res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:     "7:1:review_domain",
		Username:   "bob",
		Branch:     "1-build/review_domain/iter-1",
		BaseBranch: "1-build/develop_domain/iter-1",
		Files:      []FileWrite{{RepoRelPath: "src/domain/review.md", Content: []byte("iter-1 reject\n")}},
	})
	bob.Unlock()
	if err != nil {
		t.Fatalf("bob iter-1 submit: %v", err)
	}
	r1SHA := r1Res.CommitSHA

	// Sanity: R1's ancestry must include D1.
	if !ancestryContains(t, bob, r1SHA, d1SHA) {
		t.Fatalf("review iter-1: R1 (%s) ancestry does not contain D1 (%s) — fork-from-upstream broken at iter-1 too", r1SHA, d1SHA)
	}

	// === iter-2 ===
	// Alice re-submits develop_domain after the cascade: a NEW
	// topic branch with iter-2 in the name, with new content.
	alice.Lock()
	d2Res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:develop_domain",
		Username: "alice",
		Branch:   "1-build/develop_domain/iter-2",
		Files:    []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("package domain\n\nvar V = 2\n// iter-2 content\n")}},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice iter-2 submit: %v", err)
	}
	d2SHA := d2Res.CommitSHA

	// Bob refetches so develop_domain/iter-2 is reachable.
	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch before review iter-2: %v", err)
	}

	// THIS is the bug-reproducing call. Pre-fix this would create
	// review_domain/iter-2 forked from seed instead of from D2.
	bob.Lock()
	r2Res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:     "7:1:review_domain",
		Username:   "bob",
		Branch:     "1-build/review_domain/iter-2",
		BaseBranch: "1-build/develop_domain/iter-2",
		Files:      []FileWrite{{RepoRelPath: "src/domain/review.md", Content: []byte("iter-2 approve\n")}},
	})
	bob.Unlock()
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

// TestReviewIter2_ForksCorrectly_EvenWithStaleLocalRef stresses
// the same surface but pre-creates a stale local ref pointing at
// the seed for the would-be review iter-2 branch. This is the
// shape that bypasses CheckoutBranchFrom's BaseBranch logic via
// the "branch already exists" short-circuit at client.go:1855
// — the candidate root cause for the production reproduction.
//
// If this test fails, it confirms the short-circuit is the
// culprit and the fix is to validate the existing ref's
// ancestry against BaseBranch before honoring it.
func TestReviewIter2_ForksCorrectly_EvenWithStaleLocalRef(t *testing.T) {
	alice, bob, _ := openTwoBotClones(t)

	// alice iter-1
	alice.Lock()
	_, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:develop_domain",
		Username: "alice",
		Branch:   "1-build/develop_domain/iter-1",
		Files:    []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("v1\n")}},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice iter-1: %v", err)
	}

	// Stash bob's seed (root) commit so we can plant a stale ref.
	bobHead, err := bob.repo.Head()
	if err != nil {
		t.Fatalf("bob head: %v", err)
	}
	seedHash := bobHead.Hash()

	// alice iter-2
	alice.Lock()
	d2Res, err := alice.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:develop_domain",
		Username: "alice",
		Branch:   "1-build/develop_domain/iter-2",
		Files:    []FileWrite{{RepoRelPath: "src/domain/foo.go", Content: []byte("v2\n")}},
	})
	alice.Unlock()
	if err != nil {
		t.Fatalf("alice iter-2: %v", err)
	}
	d2SHA := d2Res.CommitSHA

	if err := bob.FetchAllRefs(); err != nil {
		t.Fatalf("bob fetch: %v", err)
	}

	// The stale-ref injection: pre-create review_domain/iter-2 at
	// seed BEFORE bob's submit attempts to fork it. This is what
	// the production clone would look like if a previous run / a
	// fetched stale tracking ref planted the same branch name.
	staleRefName := "refs/heads/1-build/review_domain/iter-2"
	if err := bob.repo.Storer.SetReference(
		plumbing.NewHashReference(plumbing.ReferenceName(staleRefName), seedHash),
	); err != nil {
		t.Fatalf("planting stale ref: %v", err)
	}

	// Now bob submits review iter-2.
	bob.Lock()
	r2Res, err := bob.SubmitTaskResult(SubmitRequest{
		TaskID:     "7:1:review_domain",
		Username:   "bob",
		Branch:     "1-build/review_domain/iter-2",
		BaseBranch: "1-build/develop_domain/iter-2",
		Files:      []FileWrite{{RepoRelPath: "review.md", Content: []byte("approve\n")}},
	})
	bob.Unlock()
	if err != nil {
		t.Fatalf("bob iter-2 submit: %v", err)
	}

	// Verify R2's ancestry includes D2 — i.e. CheckoutBranchFrom
	// did NOT short-circuit on the stale ref.
	if !ancestryContains(t, bob, r2Res.CommitSHA, d2SHA) {
		t.Fatalf("REPRO: stale local ref short-circuited fork-from-base.\n"+
			"  R2 = %s\n  D2 = %s (expected ancestor, missing)\n"+
			"  R2 ancestry: %s\n"+
			"  CheckoutBranchFrom skipped BaseBranch resolution because the branch ref already existed (pointing at seed).\n"+
			"  Fix: validate the existing ref's ancestry against BaseBranch before honoring the short-circuit.",
			r2Res.CommitSHA, d2SHA, ancestryDump(t, bob, r2Res.CommitSHA))
	}
}

// ancestryContains walks back from `head` via parents and
// returns whether `target` is reached. Used to assert one
// commit is an ancestor of another without exhausting commits
// — bounded at 50 hops which is plenty for these tests.
func ancestryContains(t *testing.T, p *Clone, head, target string) bool {
	t.Helper()
	if head == target {
		return true
	}
	visited := map[string]bool{}
	frontier := []string{head}
	for hops := 0; hops < 50 && len(frontier) > 0; hops++ {
		next := []string{}
		for _, sha := range frontier {
			if visited[sha] {
				continue
			}
			visited[sha] = true
			if sha == target {
				return true
			}
			c, err := p.repo.CommitObject(plumbing.NewHash(sha))
			if err != nil {
				continue
			}
			for _, parent := range c.ParentHashes {
				next = append(next, parent.String())
			}
		}
		frontier = next
	}
	return false
}

func ancestryDump(t *testing.T, p *Clone, head string) string {
	t.Helper()
	out := []string{}
	frontier := []string{head}
	visited := map[string]bool{}
	for hops := 0; hops < 20 && len(frontier) > 0; hops++ {
		next := []string{}
		for _, sha := range frontier {
			if visited[sha] {
				continue
			}
			visited[sha] = true
			c, err := p.repo.CommitObject(plumbing.NewHash(sha))
			if err != nil {
				out = append(out, sha+" (missing)")
				continue
			}
			subj := c.Message
			if i := indexNewline(subj); i >= 0 {
				subj = subj[:i]
			}
			out = append(out, sha[:8]+" "+subj)
			for _, parent := range c.ParentHashes {
				next = append(next, parent.String())
			}
		}
		frontier = next
	}
	return joinLines(out)
}

func indexNewline(s string) int {
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			return i
		}
	}
	return -1
}

func joinLines(xs []string) string {
	out := ""
	for _, x := range xs {
		if out != "" {
			out += "\n    "
		}
		out += x
	}
	return out
}
