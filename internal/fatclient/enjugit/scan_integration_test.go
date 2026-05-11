package enjugit

// Real-bare integration tests for Workflow.FetchBranch and
// Workflow.ScanBranchSince. The fake-ops unit-style coverage
// can't see actual go-git fetch/scan behavior; these tests pin
// the end-to-end shape against a live bare so production
// reconcile semantics stay protected.

import (
	"path/filepath"
	"strconv"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// submitTaskCommitOnMain lands one Enju-trailered commit on the
// writer's main branch and pushes it. Returns the commit SHA.
// Helper for the writer/reader scan scenarios below.
func submitTaskCommitOnMain(t *testing.T, wf *Workflow, taskID string, exitCode int) string {
	t.Helper()
	res, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         taskID,
		BranchOverride: "main",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{
				RepoRelPath: filepath.Join(resolveTestResultDir(1, "", taskID), "result.md"),
				Content:     []byte("done"),
			},
		},
		// Enju-Task-Complete is auto-emitted from req.TaskID by
		// buildSubmitTrailers; Enju-Exit isn't in the standard set
		// (compute wrappers add it via CustomTrailers), so we
		// thread it through explicitly here for parity with project
		// package's helper.
		CustomTrailers: map[string]string{
			TrailerExit: strconv.Itoa(exitCode),
		},
	})
	if err != nil {
		t.Fatalf("submit %q: %v", taskID, err)
	}
	return res.CommitSHA
}

// TestFetchBranchPureFetchNoWorktreeChange verifies that
// Workflow.FetchBranch refreshes origin/<branch> but doesn't
// touch HEAD or the worktree. Production scanner relies on this:
// it runs while users may be mid-edit in their clone, so it must
// be invisible to local state.
func TestFetchBranchPureFetchNoWorktreeChange(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	readerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	reader, err := readerWS.ForProject(1, bare)
	if err != nil {
		t.Fatalf("reader clone: %v", err)
	}
	writerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	writer, err := writerWS.ForProject(1, bare)
	if err != nil {
		t.Fatalf("writer clone: %v", err)
	}

	sha := submitTaskCommitOnMain(t, writer, "1:1:t1", 0)

	preHead, _, err := reader.Head()
	if err != nil {
		t.Fatalf("pre-head: %v", err)
	}
	if err := reader.FetchBranch("main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	postHead, _, err := reader.Head()
	if err != nil {
		t.Fatalf("post-head: %v", err)
	}
	if preHead != postHead {
		t.Errorf("HEAD must not move on fetch; pre=%s post=%s", preHead, postHead)
	}
	// origin/main has the new commit — scanning from "" returns it as tip.
	res, err := reader.ScanBranchSince("main", "")
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.NewTip != sha {
		t.Errorf("origin/main after fetch: got %s, want %s", res.NewTip, sha)
	}
}

// TestFetchBranchNonexistentRemoteBranchIsNoOp covers the
// brand-new-branch case: a topic the writer hasn't pushed yet.
// FetchBranch must NOT error — async tasks routinely target
// branches before any commit lands on them.
func TestFetchBranchNonexistentRemoteBranchIsNoOp(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	if err := wf.FetchBranch("never-pushed"); err != nil {
		t.Errorf("fetch of nonexistent branch should be no-op, got: %v", err)
	}
}

// TestScanBranchSinceBaselineAndIncremental folds project's
// baseline + incremental + no-new-commits cases into one scenario:
//   - first scan with since="" returns tip + EMPTY (no historical
//     replay, even when commits exist)
//   - second scan with same cursor returns tip + EMPTY (idempotent)
//   - new commits land; third scan with the previous tip as cursor
//     returns NEW commits in chronological order with trailer
//     content round-tripped
//
// Protects against the regression that re-processed every already-
// reconciled commit on startup.
func TestScanBranchSinceBaselineAndIncremental(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	writerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	writer, _ := writerWS.ForProject(1, bare)
	submitTaskCommitOnMain(t, writer, "1:1:t1", 0)
	submitTaskCommitOnMain(t, writer, "1:1:t2", 0)

	readerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	reader, _ := readerWS.ForProject(1, bare)
	if err := reader.FetchBranch("main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Baseline scan: tip set, no trailers returned.
	baselineRes, err := reader.ScanBranchSince("main", "")
	if err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	baseline := baselineRes.NewTip
	if baseline == "" {
		t.Fatal("expected non-empty baseline tip")
	}
	if len(baselineRes.Trailers) != 0 {
		t.Errorf("baseline scan must not replay history, got %d trailers", len(baselineRes.Trailers))
	}

	// Re-scan with the baseline cursor before any new commits:
	// idempotent no-op.
	noopRes, err := reader.ScanBranchSince("main", baseline)
	if err != nil {
		t.Fatalf("noop scan: %v", err)
	}
	if noopRes.NewTip != baseline {
		t.Errorf("noop scan should return same tip; got %s want %s", noopRes.NewTip, baseline)
	}
	if len(noopRes.Trailers) != 0 {
		t.Errorf("noop scan should return no trailers, got %d", len(noopRes.Trailers))
	}

	// Two new commits land.
	sha1 := submitTaskCommitOnMain(t, writer, "1:1:new1", 0)
	sha2 := submitTaskCommitOnMain(t, writer, "1:1:new2", 1)

	if err := reader.FetchBranch("main"); err != nil {
		t.Fatalf("re-fetch: %v", err)
	}
	incRes, err := reader.ScanBranchSince("main", baseline)
	if err != nil {
		t.Fatalf("incremental scan: %v", err)
	}
	if incRes.NewTip != sha2 {
		t.Errorf("incremental tip: got %s, want %s", incRes.NewTip, sha2)
	}
	found := incRes.Trailers
	if len(found) != 2 {
		t.Fatalf("expected 2 trailers since baseline, got %d: %+v", len(found), found)
	}
	// Chronological order so upstream completions reconcile before
	// downstream ones in the same scan window.
	if found[0].CommitSHA != sha1 || found[1].CommitSHA != sha2 {
		t.Errorf("expected [%s, %s], got [%s, %s]", sha1, sha2, found[0].CommitSHA, found[1].CommitSHA)
	}
	// Trailer content round-trips: TaskID, ExitSet, ExitCode all
	// present.
	if found[0].Trailers.TaskID != "1:1:new1" || !found[0].Trailers.ExitSet || found[0].Trailers.ExitCode != 0 {
		t.Errorf("first trailer mismatch: %+v", found[0].Trailers)
	}
	if found[1].Trailers.TaskID != "1:1:new2" || !found[1].Trailers.ExitSet || found[1].Trailers.ExitCode != 1 {
		t.Errorf("second trailer mismatch: %+v", found[1].Trailers)
	}
}

// TestScanBranchSinceSkipsNonTaskCommits: non-Enju-trailered
// commits (e.g. human pushes mixed into main) must be silently
// skipped. Without this filter, the reconcile path would fire on
// every push and surface manual edits as if they were async-task
// completions.
func TestScanBranchSinceSkipsNonTaskCommits(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	writerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	writer, _ := writerWS.ForProject(1, bare)

	taskA := submitTaskCommitOnMain(t, writer, "1:1:auto1", 0)

	// A human commit (no Enju trailers) — uses CommitArbitraryFiles
	// without a Trailers field, so ParseEnjuTrailers won't see TaskID.
	if _, err := writer.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "main",
		Subject:     "Add hand-written note",
		AuthorName:  "Alice",
		AuthorEmail: "alice@example.com",
		Files: []FileWrite{
			{RepoRelPath: "notes/human.md", Content: []byte("human notes")},
		},
	}); err != nil {
		t.Fatalf("human commit: %v", err)
	}

	taskB := submitTaskCommitOnMain(t, writer, "1:1:auto2", 0)

	readerWS, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	reader, _ := readerWS.ForProject(1, bare)
	if err := reader.FetchBranch("main"); err != nil {
		t.Fatalf("fetch: %v", err)
	}

	// Walk from BEFORE everything (the bare's seed root) so all
	// three commits are in scope. resolveSeedRoot walks the bare's
	// log to find the parentless commit.
	seedSHA := resolveSeedRoot(t, bare)
	res, err := reader.ScanBranchSince("main", seedSHA)
	if err != nil {
		t.Fatalf("scan: %v", err)
	}
	if res.NewTip != taskB {
		t.Errorf("expected tip=%s, got %s", taskB, res.NewTip)
	}
	found := res.Trailers
	if len(found) != 2 {
		t.Fatalf("expected 2 task trailers (human commit skipped), got %d: %+v", len(found), found)
	}
	if found[0].Trailers.TaskID != "1:1:auto1" {
		t.Errorf("expected first trailer auto1, got %s", found[0].Trailers.TaskID)
	}
	if found[1].Trailers.TaskID != "1:1:auto2" {
		t.Errorf("expected second trailer auto2, got %s", found[1].Trailers.TaskID)
	}
	_ = taskA
}

// TestScanBranchSinceFallsBackToLocalHeads pins the local-only
// solo mode contract: when no origin tracking ref exists, the
// scanner walks refs/heads/<branch> directly. Without this
// fallback, async-task trailers on a no-remote project would
// never reach the coordinator (TP53 Bug 1's failure mode).
//
// Constructs a workspace that local-inits without a remote, then
// hand-builds a trailer-bearing commit using go-git directly so
// no SubmitTaskResult push path is exercised.
func TestScanBranchSinceFallsBackToLocalHeads(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(1, "")
	if err != nil {
		t.Fatalf("local-init ForProject: %v", err)
	}

	// Capture baseline (the seed commit's SHA) before adding the
	// trailer commit so the cursor has a well-defined starting
	// point.
	baseline, _, err := wf.Head()
	if err != nil {
		t.Fatalf("head: %v", err)
	}

	// Build a trailer-bearing commit via CommitArbitraryFiles with
	// a custom Enju-Task-Complete trailer + Enju-Exit-Code so
	// ParseEnjuTrailers picks them up.
	res, err := wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
		Branch:      "main",
		Subject:     "Task 1:1:work by @t: result",
		AuthorName:  "T",
		AuthorEmail: "t@t",
		Files: []FileWrite{
			{RepoRelPath: "result.md", Content: []byte("ran")},
		},
		CustomTrailers: map[string]string{
			TrailerTaskComplete: "1:1:work",
		},
	})
	if err != nil {
		t.Fatalf("commit: %v", err)
	}

	scanRes, err := wf.ScanBranchSince("main", baseline)
	if err != nil {
		t.Fatalf("ScanBranchSince: %v", err)
	}
	if scanRes.NewTip != res.CommitSHA {
		t.Errorf("tip: got %s, want %s", scanRes.NewTip, res.CommitSHA)
	}
	found := scanRes.Trailers
	if len(found) != 1 {
		t.Fatalf("expected 1 trailer commit, got %d: %+v", len(found), found)
	}
	if found[0].Trailers.TaskID != "1:1:work" {
		t.Errorf("trailer task_id: got %q, want %q", found[0].Trailers.TaskID, "1:1:work")
	}
	if found[0].CommitSHA != res.CommitSHA {
		t.Errorf("trailer commit SHA: got %s, want %s", found[0].CommitSHA, res.CommitSHA)
	}
}

// TestScanBranchSinceUnknownBranchReturnsEmpty: when neither
// refs/remotes/origin/<branch> nor refs/heads/<branch> exists,
// the scanner returns the input cursor + empty results. This is
// "branch is unknown" — a legitimate state, not an error.
func TestScanBranchSinceUnknownBranchReturnsEmpty(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(1, "")
	if err != nil {
		t.Fatal(err)
	}
	res, err := wf.ScanBranchSince("does-not-exist", "abc123")
	if err != nil {
		t.Fatalf("ScanBranchSince on unknown branch: %v", err)
	}
	if res.NewTip != "abc123" {
		t.Errorf("tip should be unchanged when branch unknown; got %s, want abc123", res.NewTip)
	}
	if len(res.Trailers) != 0 {
		t.Errorf("expected no trailers from unknown branch, got %d", len(res.Trailers))
	}
}

// resolveSeedRoot walks the bare's main log back to the
// parentless commit and returns its SHA. Used by skip-non-task
// scan to start "from before anything the test created."
func resolveSeedRoot(t *testing.T, bare string) string {
	t.Helper()
	// `git rev-list --max-parents=0 main` returns every root
	// commit reachable from main (one per orphan branch, but
	// fixtures only have one root).
	root := gittest.Run(t, bare, "rev-list", "--max-parents=0", "main")
	if root == "" {
		t.Fatal("seed root not found")
	}
	return root
}

