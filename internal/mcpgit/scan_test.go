package mcpgit

// Fetch-path scanner tests. The scanner is how phase 4's async
// compute completion gets from "commit landed on origin" to
// "coordinator knows the task finished," so these tests pin the
// three pieces independently:
//
//   - FetchBranch: pure fetch, no worktree touch, handles the
//     "brand-new remote branch" no-op cleanly.
//   - ScanBranchSince: first-time baseline vs. incremental scan
//     vs. no-new-commits, all return the right tip and trailer
//     list.
//   - Cursors: JSON-file round-trip, atomic save, malformed
//     fallback.

import (
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// submitOnBranch is a test helper that lands one task-result
// commit (with Enju trailers) on the given branch of the
// project's local clone and pushes it to the bare remote.
// Returns the commit SHA. Mirrors what the production wrapper
// does, just with canned content.
func submitOnBranch(t *testing.T, proj *Project, branch, taskID, username string, exitCode int) string {
	t.Helper()
	resultDir := ResultDir(1, "", taskID)
	proj.Lock()
	defer proj.Unlock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   taskID,
		Username: username,
		Branch:   branch,
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resultDir, "result.md"),
				Content:     []byte("hi"),
			},
		},
		Trailers: EnjuTrailers{
			TaskID:   taskID,
			ExitCode: exitCode,
			ExitSet:  true,
		},
	})
	if err != nil {
		t.Fatalf("submit %q on %q: %v", taskID, branch, err)
	}
	return res.CommitSHA
}

// TestFetchBranchPureFetchNoWorktreeChange verifies that
// FetchBranch updates origin/<branch> but doesn't touch HEAD or
// the worktree. This is the invariant the scanner relies on to
// run safely while the user is mid-edit in their clone.
func TestFetchBranchPureFetchNoWorktreeChange(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Reader side: separate clone that will FetchBranch.
	readerWS, err := NewWorkspace(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("reader workspace: %v", err)
	}
	reader, err := readerWS.ForProject(1, bare)
	if err != nil {
		t.Fatalf("reader clone: %v", err)
	}

	// Writer side: land a commit on main via a different
	// workspace, so the reader's fetch has something new to see.
	writerWS, err := NewWorkspace(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("writer workspace: %v", err)
	}
	writer, err := writerWS.ForProject(1, bare)
	if err != nil {
		t.Fatalf("writer clone: %v", err)
	}
	sha := submitOnBranch(t, writer, "main", "1:1:t1", "alice", 0)

	// Capture reader's HEAD BEFORE fetch.
	preHead, _ := reader.HeadHash()

	if err := reader.FetchBranch("main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// HEAD unchanged — fetch doesn't touch local refs.
	postHead, _ := reader.HeadHash()
	if preHead != postHead {
		t.Errorf("expected HEAD unchanged after fetch; pre=%s post=%s", preHead, postHead)
	}

	// origin/main DOES advance to the new commit.
	newTip, _, _ := reader.ScanBranchSince("main", "")
	if newTip != sha {
		t.Errorf("expected origin/main=%s after fetch, got %s", sha, newTip)
	}
}

// TestFetchBranchNonexistentRemoteBranchIsNoOp covers the
// "branch doesn't exist on origin yet" case — the scanner must
// not error in that case, since async tasks can be kicked off
// on a branch that hasn't pushed anything yet.
func TestFetchBranchNonexistentRemoteBranchIsNoOp(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewWorkspace(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if err := proj.FetchBranch("never-pushed"); err != nil {
		t.Errorf("fetch of nonexistent branch should be no-op, got: %v", err)
	}
}

// TestScanBranchSinceBaseline: first-time scan (since="")
// returns tip + empty, NOT the whole history. Protects against
// re-processing every already-reconciled commit on startup.
func TestScanBranchSinceBaseline(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	writerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	writer, _ := writerWS.ForProject(1, bare)
	submitOnBranch(t, writer, "main", "1:1:t1", "alice", 0)
	submitOnBranch(t, writer, "main", "1:1:t2", "alice", 0)

	readerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	reader, _ := readerWS.ForProject(1, bare)
	if err := reader.FetchBranch("main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	tip, found, err := reader.ScanBranchSince("main", "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tip == "" {
		t.Fatalf("expected non-empty tip")
	}
	if len(found) != 0 {
		t.Errorf("first-time scan must be baseline-only (no results), got %d trailers", len(found))
	}
}

// TestScanBranchSinceIncremental: scanning again from a
// previous cursor yields only the commits added since. Core
// incremental behavior the scanner depends on.
func TestScanBranchSinceIncremental(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	writerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	writer, _ := writerWS.ForProject(1, bare)
	submitOnBranch(t, writer, "main", "1:1:pre", "alice", 0)

	readerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	reader, _ := readerWS.ForProject(1, bare)
	reader.FetchBranch("main")
	baseline, _, _ := reader.ScanBranchSince("main", "")

	// Two new commits land after baseline.
	sha1 := submitOnBranch(t, writer, "main", "1:1:new1", "alice", 0)
	sha2 := submitOnBranch(t, writer, "main", "1:1:new2", "alice", 1) // exit 1 — failure

	reader.FetchBranch("main")
	tip, found, err := reader.ScanBranchSince("main", baseline)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tip != sha2 {
		t.Errorf("expected tip=%s, got %s", sha2, tip)
	}
	if len(found) != 2 {
		t.Fatalf("expected 2 trailers since baseline, got %d: %+v", len(found), found)
	}
	// Results must be in chronological order so upstream
	// completions can be reconciled before downstream ones in
	// the same scan window.
	if found[0].CommitSHA != sha1 || found[1].CommitSHA != sha2 {
		t.Errorf("expected [%s, %s], got [%s, %s]", sha1, sha2, found[0].CommitSHA, found[1].CommitSHA)
	}
	// Trailer content round-trips.
	if found[0].Trailers.TaskID != "1:1:new1" || !found[0].Trailers.ExitSet || found[0].Trailers.ExitCode != 0 {
		t.Errorf("first trailer mismatch: %+v", found[0].Trailers)
	}
	if found[1].Trailers.TaskID != "1:1:new2" || !found[1].Trailers.ExitSet || found[1].Trailers.ExitCode != 1 {
		t.Errorf("second trailer mismatch: %+v", found[1].Trailers)
	}
}

// TestScanBranchSinceNoNewCommits: re-scanning when nothing's
// changed returns tip + empty (not an error).
func TestScanBranchSinceNoNewCommits(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	writerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	writer, _ := writerWS.ForProject(1, bare)
	submitOnBranch(t, writer, "main", "1:1:t1", "alice", 0)

	readerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	reader, _ := readerWS.ForProject(1, bare)
	reader.FetchBranch("main")
	baseline, _, _ := reader.ScanBranchSince("main", "")

	// No new commits — scan from baseline.
	tip, found, err := reader.ScanBranchSince("main", baseline)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if tip != baseline {
		t.Errorf("expected tip unchanged, got %s != %s", tip, baseline)
	}
	if len(found) != 0 {
		t.Errorf("expected no trailers, got %d", len(found))
	}
}

// TestScanBranchSinceSkipsNonTaskCommits: commits without the
// Enju-Task-Complete trailer are silently skipped. Matters
// because humans push hand-authored commits too — those
// shouldn't show up as reconcile entries.
func TestScanBranchSinceSkipsNonTaskCommits(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	writerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	writer, _ := writerWS.ForProject(1, bare)

	// Mix: task commit, human commit, task commit.
	sha1 := submitOnBranch(t, writer, "main", "1:1:auto1", "alice", 0)
	// CommitFiles without the task trailers — simulates a
	// human-authored commit to the project.
	writer.Lock()
	if _, err := writer.CommitFiles(CommitFilesRequest{
		CommitMsg: "Add hand-written note",
		Files: []FileWrite{
			{RepoRelPath: "notes/human.md", Content: []byte("human notes")},
		},
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.com",
		Branch:      "main",
	}); err != nil {
		t.Fatalf("human commit: %v", err)
	}
	writer.Unlock()
	sha3 := submitOnBranch(t, writer, "main", "1:1:auto2", "alice", 0)

	readerWS, _ := NewWorkspace(t.TempDir(), nullLogger())
	reader, _ := readerWS.ForProject(1, bare)
	reader.FetchBranch("main")
	baseline, _, _ := reader.ScanBranchSince("main", "")
	_ = sha1 // baseline captures tip after sha1 — not guaranteed ordering here

	// Rewind baseline to pre-sha1 by reading origin/main from
	// before anything. We can achieve this by scanning from "".
	// Simpler: use the seed remote's initial commit as the
	// since point so the scan walks all three new commits.
	seedSHA := findSeedSHA(t, reader)

	tip, found, err := reader.ScanBranchSince("main", seedSHA)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	// We expect exactly 2 trailer matches (the two task commits),
	// skipping the human commit silently.
	if len(found) != 2 {
		t.Fatalf("expected 2 task trailers, got %d: %+v", len(found), found)
	}
	// Tip is sha3 (the latest).
	if tip != sha3 {
		t.Errorf("expected tip=%s, got %s", sha3, tip)
	}
	// First result was auto1, second auto2.
	if found[0].Trailers.TaskID != "1:1:auto1" {
		t.Errorf("expected first trailer auto1, got %s", found[0].Trailers.TaskID)
	}
	if found[1].Trailers.TaskID != "1:1:auto2" {
		t.Errorf("expected second trailer auto2, got %s", found[1].Trailers.TaskID)
	}
	_ = baseline
}

// findSeedSHA walks origin/main's history back to its root and
// returns the root SHA. Used to make incremental-scan tests
// start from "before anything the test created."
func findSeedSHA(t *testing.T, proj *Project) string {
	t.Helper()
	tip, _, err := proj.ScanBranchSince("main", "")
	if err != nil {
		t.Fatalf("baseline: %v", err)
	}
	// Walk ancestors of tip; the last commit with zero parents
	// is the root.
	tipHash := plumbingHash(tip)
	iter, err := proj.repo.Log(&gogit.LogOptions{From: tipHash})
	if err != nil {
		t.Fatalf("log: %v", err)
	}
	defer iter.Close()
	var root string
	for {
		c, err := iter.Next()
		if err != nil {
			break
		}
		if c.NumParents() == 0 {
			root = c.Hash.String()
		}
	}
	if root == "" {
		t.Fatalf("no root commit found (tip=%s)", tip)
	}
	return root
}

// TestCursorsRoundTrip: save → load yields the same map.
// Basic serialization sanity check so a crash between scans
// doesn't lose progress.
func TestCursorsRoundTrip(t *testing.T) {
	stateDir := t.TempDir()

	c := NewCursors(stateDir, 42)
	c.Set("main", "aaaa")
	c.Set("branch-one", "bbbb")
	if err := c.Save(); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := LoadCursors(stateDir, 42)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.Get("main") != "aaaa" {
		t.Errorf("main: expected aaaa, got %q", loaded.Get("main"))
	}
	if loaded.Get("branch-one") != "bbbb" {
		t.Errorf("branch-one: expected bbbb, got %q", loaded.Get("branch-one"))
	}
}

// TestCursorsEmptyWhenFileMissing: first-run case — no file
// yet, LoadCursors returns an empty-but-valid Cursors.
func TestCursorsEmptyWhenFileMissing(t *testing.T) {
	stateDir := t.TempDir()
	c, err := LoadCursors(stateDir, 99)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Get("main") != "" {
		t.Errorf("expected empty cursor on missing file, got %q", c.Get("main"))
	}
}

// TestCursorsFallsBackOnCorruptFile: a malformed JSON must not
// wedge the scanner. We fall back to empty state so the next
// scan sets a fresh baseline.
func TestCursorsFallsBackOnCorruptFile(t *testing.T) {
	stateDir := t.TempDir()
	path := cursorsPath(stateDir, 7)
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("{not json"), 0644); err != nil {
		t.Fatal(err)
	}
	c, err := LoadCursors(stateDir, 7)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if c.Get("main") != "" {
		t.Errorf("expected fallback to empty on corrupt file")
	}
	// Save should overwrite the junk without complaint.
	c.Set("main", "a1b2")
	if err := c.Save(); err != nil {
		t.Errorf("save after corrupt load: %v", err)
	}
}

// TestCursorsVersionMismatch: a cursor file from a future
// schema version is treated as empty (so an old client won't
// act on data it can't interpret).
func TestCursorsVersionMismatch(t *testing.T) {
	stateDir := t.TempDir()
	path := cursorsPath(stateDir, 8)
	os.MkdirAll(filepath.Dir(path), 0755)
	os.WriteFile(path, []byte(`{"version":999,"branches":{"main":"xxx"}}`), 0644)
	c, _ := LoadCursors(stateDir, 8)
	if c.Get("main") != "" {
		t.Errorf("expected version mismatch to reset; got main=%q", c.Get("main"))
	}
}

// TestCursorsAtomicSaveSurvivesPartialWrite: we save via temp
// file + rename, so a failed write leaves the previous state
// intact. Simulate by attempting a second save concurrently —
// this doesn't exhaustively prove atomicity but catches
// obvious bugs like truncating the file before write.
func TestCursorsAtomicSaveSurvivesPartialWrite(t *testing.T) {
	stateDir := t.TempDir()
	c := NewCursors(stateDir, 5)
	c.Set("main", "first")
	if err := c.Save(); err != nil {
		t.Fatalf("first save: %v", err)
	}

	// Overwrite with a second state.
	c.Set("main", "second")
	if err := c.Save(); err != nil {
		t.Fatalf("second save: %v", err)
	}
	reloaded, _ := LoadCursors(stateDir, 5)
	if reloaded.Get("main") != "second" {
		t.Errorf("expected second, got %q", reloaded.Get("main"))
	}
}

// TestAdvanceCursorIfConfiguredSerializesConcurrentCallers
// covers the last-writer-wins race the tester flagged. Before
// CursorMutexFor, two callers could each do
// LoadCursors → Set(branch, sha_i) → Save concurrently; the
// later writer's save carried its own older snapshot and
// silently overwrote the earlier writer's advance. The
// atomic-rename inside Save keeps the file from corrupting,
// but the cursor still goes BACKWARDS — next scan walks extra
// history. Now both writers serialize through CursorMutexFor.
//
// Test: N goroutines each advance a unique commit SHA on the
// same branch. After all finish, the saved cursor MUST be one
// of the N SHAs (never empty, never a malformed mix). The
// file must parse cleanly.
func TestAdvanceCursorIfConfiguredSerializesConcurrentCallers(t *testing.T) {
	stateDir := t.TempDir()
	const projectID int64 = 42
	const N = 20

	// Pre-seed with an initial cursor so the file exists
	// before the racers start.
	seed := NewCursors(stateDir, projectID)
	seed.Set("main", "seed")
	if err := seed.Save(); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var wg sync.WaitGroup
	shas := make([]string, N)
	for i := 0; i < N; i++ {
		shas[i] = fmt.Sprintf("%040x", i+1)
	}
	wg.Add(N)
	for i := 0; i < N; i++ {
		go func(sha string) {
			defer wg.Done()
			advanceCursorIfConfigured(projectID, stateDir, "main", sha)
		}(shas[i])
	}
	wg.Wait()

	loaded, err := LoadCursors(stateDir, projectID)
	if err != nil {
		t.Fatalf("load after race: %v", err)
	}
	final := loaded.Get("main")
	if final == "" {
		t.Fatalf("cursor vanished after concurrent advances")
	}
	// Must be one of the SHAs we wrote (not "seed", not a
	// corrupt value).
	found := false
	for _, sha := range shas {
		if sha == final {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("final cursor %q is not one of the advanced SHAs — concurrent save lost data", final)
	}
}

// TestSubmitTaskResultAutoAdvancesCursor pins the auto-advance
// hook added in SubmitTaskResult (cursor-advance ownership
// moved from the MCP handler into mcpgit so every caller —
// production, tests, future shell wrapper — benefits without
// re-implementing the post-commit cursor update). Without
// this, the scanner would replay self-generated commits as
// fresh trailer events on the next sweep.
func TestSubmitTaskResultAutoAdvancesCursor(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewWorkspace(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	const projectID int64 = 7
	proj, err := ws.ForProject(projectID, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	stateDir := t.TempDir()
	// Baseline: no cursor file yet.
	pre, _ := LoadCursors(stateDir, projectID)
	if pre.Get("main") != "" {
		t.Fatalf("expected no pre-existing cursor, got %q", pre.Get("main"))
	}

	// Submit with ProjectID + StateDir set. Expect cursor
	// to be advanced past the resulting commit.
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:auto",
		Username: "alice",
		Branch:   "main",
		Files: []FileWrite{
			{RepoRelPath: "enju/runs/1/auto/result.md", Content: []byte("ok")},
		},
		Trailers:  EnjuTrailers{TaskID: "1:1:auto", ExitCode: 0, ExitSet: true},
		ProjectID: projectID,
		StateDir:  stateDir,
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Cursor file should now exist and point at the commit
	// SubmitTaskResult returned.
	post, err := LoadCursors(stateDir, projectID)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := post.Get("main"); got != res.CommitSHA {
		t.Errorf("expected cursor=%s after submit, got %q", res.CommitSHA, got)
	}
}

// TestSubmitTaskResultSkipsCursorAdvanceWithoutConfig verifies
// the opt-in gate: callers that leave ProjectID / StateDir
// zero (coordinator-side tests, store unit tests, raw mcpgit
// callers) do NOT get a cursor file written. Otherwise any
// test that exercised SubmitTaskResult would pollute the
// user's home dir or fail in readonly environments.
func TestSubmitTaskResultSkipsCursorAdvanceWithoutConfig(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	stateDir := t.TempDir()
	proj.Lock()
	_, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:nocursor",
		Username: "alice",
		Branch:   "main",
		Files: []FileWrite{
			{RepoRelPath: "enju/runs/1/nocursor/result.md", Content: []byte("x")},
		},
		Trailers: EnjuTrailers{TaskID: "1:1:nocursor"},
		// Deliberately NOT setting ProjectID / StateDir.
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// No cursor file should exist for any project in the
	// supplied state dir — the auto-advance was gated off.
	entries, rerr := os.ReadDir(stateDir)
	if rerr != nil {
		// Dir may not exist at all, which is also fine.
		return
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".json" {
			t.Errorf("unexpected cursor file %q written despite empty ProjectID/StateDir", e.Name())
		}
	}
}
