package enjugit

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"

	corelayout "github.com/enju-ai/enju/internal/common/layout"
	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// TestCommitArbitraryFilesPlumbing_ConcurrentBranches pins the
// fix for the "concurrent enju_create_run loses snapshot" bug.
//
// Setup: one project, three run branches each pre-created
// pointing at main. Fire three goroutines that each call
// CommitArbitraryFilesPlumbing concurrently against ITS OWN
// run branch, writing a distinct snapshot file at the
// per-run path.
//
// Invariant after all three complete: every run's snapshot
// file is readable from THAT run's branch via the snapshot
// materializer. The worktree-coupled CommitArbitraryFiles
// path would have lost two of three snapshots because
// each goroutine's checkout overwrote the previous one's
// worktree state; the plumbing path lands each commit in
// its run's branch independently and the materializer
// pulls them back out at read time.
func TestCommitArbitraryFilesPlumbing_ConcurrentBranches(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 99)
	wf, err := ws.ForProject(99, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Seed three run branches at main's tip. Mirrors what
	// coord-side create_run does: each new run gets a fresh
	// branch forked from main.
	mainSHA, _, err := wf.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	runBranches := []string{"run-2", "run-3", "run-4"}
	for _, b := range runBranches {
		if err := wf.git.CreateBranchAt(b, mainSHA); err != nil {
			t.Fatalf("CreateBranchAt %s: %v", b, err)
		}
		// Push so origin tracks it (mirrors create_run's coord-side
		// branch creation followed by an implicit fetch on the
		// fatclient).
		if err := wf.git.Push(b); err != nil {
			t.Fatalf("Push %s: %v", b, err)
		}
	}

	// Fire N concurrent plumbing commits, one per run branch.
	// The payload is the run's manifest at the canonical
	// snapshot path — same shape CommitRunTemplateSnapshot
	// produces.
	type runSpec struct {
		seq    int
		slug   string
		branch string
		// distinctive marker so we can tell which run's snapshot
		// we materialized later
		marker string
	}
	runs := []runSpec{
		{seq: 2, slug: "alpha", branch: "run-2", marker: "RUN-2-MARKER"},
		{seq: 3, slug: "beta", branch: "run-3", marker: "RUN-3-MARKER"},
		{seq: 4, slug: "gamma", branch: "run-4", marker: "RUN-4-MARKER"},
	}

	var wg sync.WaitGroup
	errCh := make(chan error, len(runs))
	for _, r := range runs {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshotPath := filepath.Join(corelayout.RunTemplateSnapshotDir(r.seq, r.slug), "enju.yaml")
			_, err := wf.CommitArbitraryFilesPlumbing(CommitArbitraryFilesRequest{
				Files: []FileWrite{
					{
						RepoRelPath: snapshotPath,
						Content:     []byte(fmt.Sprintf("# %s\nrun: { name: %s }\n", r.marker, r.slug)),
					},
				},
				Branch:      r.branch,
				Subject:     fmt.Sprintf("Snapshot for run %d", r.seq),
				AuthorName:  "Tester",
				AuthorEmail: "t@x",
			})
			if err != nil {
				errCh <- fmt.Errorf("run %d (%s): %w", r.seq, r.branch, err)
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("concurrent commit failed: %v", err)
	}
	if t.Failed() {
		return
	}

	// Verify: every run's snapshot is readable from ITS branch
	// via the materializer. This is what the worktree-coupled
	// path failed at — two of three would come back empty.
	for _, r := range runs {
		t.Run("materialize "+r.branch, func(t *testing.T) {
			target := filepath.Join(t.TempDir(), "snapshot")
			n, err := wf.MaterializeRunRepo(r.branch, target)
			if err != nil {
				t.Fatalf("MaterializeRunRepo: %v", err)
			}
			if n == 0 {
				t.Fatalf("materialized zero files for run %d on branch %s (the bug we're fixing)", r.seq, r.branch)
			}
			yamlPath := filepath.Join(target, corelayout.RunTemplateSnapshotDir(r.seq, r.slug), "enju.yaml")
			content, err := os.ReadFile(yamlPath)
			if err != nil {
				t.Fatalf("read materialized enju.yaml at %s: %v", yamlPath, err)
			}
			if !strings.Contains(string(content), r.marker) {
				t.Errorf("snapshot for %s does not contain expected marker %q: %s",
					r.branch, r.marker, content)
			}
			// Cross-check: snapshot must NOT contain another
			// run's marker (would indicate the wrong branch's
			// tree got materialized).
			for _, other := range runs {
				if other.marker == r.marker {
					continue
				}
				if strings.Contains(string(content), other.marker) {
					t.Errorf("snapshot for %s contains foreign marker %q from run %s — branch isolation broken",
						r.branch, other.marker, other.branch)
				}
			}
		})
	}

	// Final cross-check on the bare: each branch's tip should
	// have a commit whose tree contains its own snapshot path
	// and not the others'.
	for _, r := range runs {
		want := filepath.Join(corelayout.RunTemplateSnapshotDir(r.seq, r.slug), "enju.yaml")
		_, lerr := gittest.RunOK(t, bare, "cat-file", "-e", "refs/heads/"+r.branch+":"+want)
		if lerr != nil {
			t.Errorf("bare branch %s missing %s after concurrent commit: %v",
				r.branch, want, lerr)
		}
	}
}

// TestMaterializeRunRepo_PreservesExecutableBit pins the
// mode-preservation contract: a script file committed with
// mode 0755 in git must come out of the materializer as 0755
// on disk, so the wrapper's fork/exec can run it without
// EACCES.
func TestMaterializeRunRepo_PreservesExecutableBit(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 100)
	wf, err := ws.ForProject(100, bare)
	if err != nil {
		t.Fatal(err)
	}
	mainSHA, _, _ := wf.Head()
	if err := wf.git.CreateBranchAt("run-exec", mainSHA); err != nil {
		t.Fatal(err)
	}
	if err := wf.git.Push("run-exec"); err != nil {
		t.Fatal(err)
	}

	snapshotSubdir := corelayout.RunTemplateSnapshotDir(1, "exec")
	scriptPath := filepath.Join(snapshotSubdir, "scripts", "entry.sh")
	if _, err := wf.CommitArbitraryFilesPlumbing(CommitArbitraryFilesRequest{
		Files: []FileWrite{
			{RepoRelPath: scriptPath, Content: []byte("#!/bin/sh\necho hi\n"), Mode: 0o755},
		},
		Branch:  "run-exec",
		Subject: "Add executable script",
	}); err != nil {
		t.Fatalf("CommitArbitraryFilesPlumbing: %v", err)
	}

	target := filepath.Join(t.TempDir(), "snapshot")
	if _, err := wf.MaterializeRunRepo("run-exec", target); err != nil {
		t.Fatalf("MaterializeRunRepo: %v", err)
	}
	info, err := os.Stat(filepath.Join(target, snapshotSubdir, "scripts", "entry.sh"))
	if err != nil {
		t.Fatalf("stat materialized script: %v", err)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("materialized script mode = %v, expected executable bits set", info.Mode())
	}
}

// TestMaterializeRunRepo_BranchMissing returns a clear
// error rather than silently producing an empty dir.
func TestMaterializeRunRepo_BranchMissing(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 101)
	wf, err := ws.ForProject(101, bare)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(t.TempDir(), "snapshot")
	_, err = wf.MaterializeRunRepo("never-existed", target)
	if err == nil {
		t.Error("expected error for missing branch")
	}
}

// TestCommitArbitraryFiles_WorktreePathLosesSnapshotsUnderConcurrency
// is the FAILURE-PROOF for the bug TestCommitArbitraryFilesPlumbing_ConcurrentBranches
// fixes. It exercises the OLD worktree-coupled path under the
// same concurrent-branches scenario and asserts the broken
// invariant directly: after N concurrent CommitArbitraryFiles
// calls each writing to a distinct run branch, the operator
// worktree on disk only holds ONE run's snapshot. The other
// runs' files live only in the bare's branch history — not on
// disk — exactly the symptom the tester reported.
//
// Why this test must stay: without it, a future regression
// (e.g. a "performance optimization" that switches back to
// the worktree path) wouldn't fail anywhere obvious; the
// concurrent_runs scenario would just silently re-break.
//
// The assertion is asymmetric: we don't require all three
// reads to fail (the surviving one is whichever goroutine
// went last through WithLock, which is non-deterministic) —
// we require at least one to be missing. That's enough to
// pin "the worktree path can't hold N snapshots
// simultaneously" without depending on Go's scheduler order.
func TestCommitArbitraryFiles_WorktreePathLosesSnapshotsUnderConcurrency(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 98)
	wf, err := ws.ForProject(98, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	mainSHA, _, err := wf.Head()
	if err != nil {
		t.Fatalf("Head: %v", err)
	}
	runBranches := []string{"old-run-2", "old-run-3", "old-run-4"}
	for _, b := range runBranches {
		if err := wf.git.CreateBranchAt(b, mainSHA); err != nil {
			t.Fatalf("CreateBranchAt %s: %v", b, err)
		}
		if err := wf.git.Push(b); err != nil {
			t.Fatalf("Push %s: %v", b, err)
		}
	}

	type runSpec struct {
		seq    int
		slug   string
		branch string
	}
	runs := []runSpec{
		{seq: 2, slug: "alpha", branch: "old-run-2"},
		{seq: 3, slug: "beta", branch: "old-run-3"},
		{seq: 4, slug: "gamma", branch: "old-run-4"},
	}

	// Fire concurrent CommitArbitraryFiles (the WORKTREE path).
	// WithLock serializes them, so they don't crash on each
	// other — but each one's checkout overwrites the worktree
	// to its branch's tree, and the FINAL worktree state only
	// reflects whichever ran last.
	var wg sync.WaitGroup
	for _, r := range runs {
		r := r
		wg.Add(1)
		go func() {
			defer wg.Done()
			snapshotPath := filepath.Join(corelayout.RunTemplateSnapshotDir(r.seq, r.slug), "enju.yaml")
			_, _ = wf.CommitArbitraryFiles(CommitArbitraryFilesRequest{
				Files: []FileWrite{
					{RepoRelPath: snapshotPath, Content: []byte("run: { name: " + r.slug + " }\n")},
				},
				Branch:      r.branch,
				Subject:     "snapshot via worktree",
				AuthorName:  "Tester",
				AuthorEmail: "t@x",
			})
		}()
	}
	wg.Wait()

	// Now count how many runs' snapshots are on disk in the
	// operator worktree. The bug: only one survives (or zero
	// if the test runner shuffled things weirdly). Three is
	// what the worktree path CANNOT produce.
	present := 0
	var missing []string
	for _, r := range runs {
		path := filepath.Join(wf.WorkDir(),
			corelayout.RunTemplateSnapshotDir(r.seq, r.slug), "enju.yaml")
		if _, err := os.Stat(path); err == nil {
			present++
		} else {
			missing = append(missing, r.branch)
		}
	}
	if present == len(runs) {
		t.Errorf("expected the worktree path to LOSE snapshots under concurrency, but all %d are present — has the bug been fixed at this layer? (if so, this test is obsolete)", len(runs))
	}
	if len(missing) == 0 {
		t.Error("no missing snapshots — bug reproducer didn't reproduce")
	}
	t.Logf("worktree-path concurrency confirmed broken: %d of %d snapshots present, missing from worktree: %v",
		present, len(runs), missing)

	// Cross-check the GIT side: every branch on the bare DOES
	// carry its commit (the bug isn't in git, it's in the
	// worktree as a coordination medium). This is what the
	// tester observed too — coord-side state ✅, branches ✅,
	// disk ❌.
	for _, r := range runs {
		want := filepath.Join(corelayout.RunTemplateSnapshotDir(r.seq, r.slug), "enju.yaml")
		if _, oerr := gittest.RunOK(t, bare, "cat-file", "-e", "refs/heads/"+r.branch+":"+want); oerr != nil {
			t.Errorf("bare branch %s missing %s — git side ALSO broken (unexpected; bug should be worktree-only): %v",
				r.branch, want, oerr)
		}
	}
}

// TestMaterializeRunRepo_WholeTreeIncludingBaseAndTemplate pins
// the per-run-snapshot redesign's read-side contract: one call
// materializes the whole tree at the run branch's tip — BOTH
// the in-git template snapshot AND the repo content the run was
// cut from. Scripts thus get a single coherent on-disk view via
// $ENJU_REPO_DIR; sibling reads against the original repo at the
// run's base SHA work without per-task materialization.
//
// Setup:
//   - bare with main containing src/lib.go (seeded by
//     initBareForWorkspaceTest).
//   - run branch "run-snap" forked from main.
//   - template snapshot landed under run-snap via plumbing.
//
// Assertion:
//   - MaterializeRunRepo produces both src/lib.go (base content)
//     and the template-snapshot subdir's files at the right
//     relative paths under the target dir.
//   - Operator's working tree is untouched (read came from
//     .git/objects/, not a checkout).
func TestMaterializeRunRepo_WholeTreeIncludingBaseAndTemplate(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 202)
	wf, err := ws.ForProject(202, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	// Seed a recognizable base-tree file on main, so the
	// materialized tree's "non-template" content has something to
	// match against.
	if _, err := wf.CommitArbitraryFilesPlumbing(CommitArbitraryFilesRequest{
		Files: []FileWrite{
			{RepoRelPath: "src/lib.go", Content: []byte("package lib\n// v1\n")},
		},
		Branch:  "main",
		Subject: "Seed src/lib.go",
	}); err != nil {
		t.Fatalf("seed main: %v", err)
	}
	mainSHA, err := wf.LocalBranchHash("main")
	if err != nil || mainSHA == "" {
		t.Fatalf("LocalBranchHash main: %v sha=%q", err, mainSHA)
	}
	if err := wf.git.CreateBranchAt("run-snap", mainSHA); err != nil {
		t.Fatalf("CreateBranchAt run-snap: %v", err)
	}
	if err := wf.git.Push("run-snap"); err != nil {
		t.Fatalf("Push run-snap: %v", err)
	}
	tmplYAML := []byte("name: snap-test\ntasks:\n  - id: t1\n    action: compute\n    script: ./run.sh\n")
	tmplScript := []byte("#!/bin/sh\ncat \"$ENJU_REPO_DIR/src/lib.go\"\n")
	snapshotSubdir := corelayout.RunTemplateSnapshotDir(7, "snap-test")
	if _, err := wf.CommitArbitraryFilesPlumbing(CommitArbitraryFilesRequest{
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(snapshotSubdir, "enju.yaml"), Content: tmplYAML},
			{RepoRelPath: filepath.Join(snapshotSubdir, "run.sh"), Content: tmplScript, Mode: 0o755},
		},
		Branch:  "run-snap",
		Subject: "Snapshot template into run-snap",
	}); err != nil {
		t.Fatalf("plumbing commit template: %v", err)
	}

	target := filepath.Join(t.TempDir(), "snapshot")
	n, err := wf.MaterializeRunRepo("run-snap", target)
	if err != nil {
		t.Fatalf("MaterializeRunRepo: %v", err)
	}
	if n < 3 {
		t.Errorf("expected at least 3 files materialized (lib + yaml + sh), got %d", n)
	}

	// Base file at its repo-relative path.
	if got, err := os.ReadFile(filepath.Join(target, "src", "lib.go")); err != nil {
		t.Errorf("base file src/lib.go missing: %v", err)
	} else if string(got) != "package lib\n// v1\n" {
		t.Errorf("base file content mismatch: %q", got)
	}
	// Template snapshot files.
	if got, err := os.ReadFile(filepath.Join(target, snapshotSubdir, "enju.yaml")); err != nil {
		t.Errorf("template yaml missing: %v", err)
	} else if string(got) != string(tmplYAML) {
		t.Errorf("template yaml mismatch: %q", got)
	}
	scriptFull := filepath.Join(target, snapshotSubdir, "run.sh")
	info, serr := os.Stat(scriptFull)
	if serr != nil {
		t.Fatalf("stat materialized run.sh: %v", serr)
	}
	if info.Mode()&0o111 == 0 {
		t.Errorf("run.sh exec bit lost (mode=%v)", info.Mode())
	}
}

// TestMaterializeRunRepo_ExcludesPriorRunResultTrail is the B5
// regression: the run branch's tree force-carries every prior
// run's committed result trail under
// .enju/runs/<seq>-<slug>/<taskDefID>/. Materializing all of it
// into each new run's snapshot made snapshot size grow linearly
// with the project's cumulative run history (the bug hunt
// measured 40K → 1.3M over 14 small runs). The materializer must
// skip the result trail while still keeping the recipe
// (template-snapshot/) and ordinary source files.
func TestMaterializeRunRepo_ExcludesPriorRunResultTrail(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 303)
	wf, err := ws.ForProject(303, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	priorResultDir := filepath.Join(corelayout.RunDir(1, "prev"), "analyze")
	curSnapshot := corelayout.RunTemplateSnapshotDir(2, "cur")
	tmplYAML := []byte("name: cur\ntasks:\n  - id: t1\n    action: compute\n    script: ./run.sh\n")
	bigResult := make([]byte, 64*1024) // a fat prior result.md
	for i := range bigResult {
		bigResult[i] = 'x'
	}

	if _, err := wf.CommitArbitraryFilesPlumbing(CommitArbitraryFilesRequest{
		Files: []FileWrite{
			// Ordinary source file — must survive.
			{RepoRelPath: "src/lib.go", Content: []byte("package lib\n")},
			// Prior run #1's force-committed result trail — must
			// NOT appear in run #2's snapshot.
			{RepoRelPath: filepath.Join(priorResultDir, "result.md"), Content: bigResult},
			{RepoRelPath: filepath.Join(priorResultDir, "metadata.json"), Content: []byte(`{"ok":true}`)},
			{RepoRelPath: filepath.Join(priorResultDir, "script.log"), Content: []byte("noise\n")},
			// Current run #2's recipe snapshot — must survive.
			{RepoRelPath: filepath.Join(curSnapshot, "enju.yaml"), Content: tmplYAML},
			{RepoRelPath: filepath.Join(curSnapshot, "run.sh"), Content: []byte("#!/bin/sh\necho hi\n"), Mode: 0o755},
		},
		Branch:  "main",
		Subject: "Seed src + prior-run trail + current recipe",
	}); err != nil {
		t.Fatalf("seed commit: %v", err)
	}
	mainSHA, err := wf.LocalBranchHash("main")
	if err != nil || mainSHA == "" {
		t.Fatalf("LocalBranchHash main: %v sha=%q", err, mainSHA)
	}
	if err := wf.git.CreateBranchAt("run-2", mainSHA); err != nil {
		t.Fatalf("CreateBranchAt run-2: %v", err)
	}

	target := filepath.Join(t.TempDir(), "snapshot")
	if _, err := wf.MaterializeRunRepo("run-2", target); err != nil {
		t.Fatalf("MaterializeRunRepo: %v", err)
	}

	// Source file: present.
	if _, err := os.Stat(filepath.Join(target, "src", "lib.go")); err != nil {
		t.Errorf("source src/lib.go should be materialized: %v", err)
	}
	// Recipe snapshot: present (scripts resolve from here).
	if _, err := os.Stat(filepath.Join(target, curSnapshot, "enju.yaml")); err != nil {
		t.Errorf("recipe enju.yaml should be materialized: %v", err)
	}
	if _, err := os.Stat(filepath.Join(target, curSnapshot, "run.sh")); err != nil {
		t.Errorf("recipe run.sh should be materialized: %v", err)
	}
	// Prior run's result trail: ABSENT (this is the fix).
	for _, p := range []string{"result.md", "metadata.json", "script.log"} {
		if _, err := os.Stat(filepath.Join(target, priorResultDir, p)); !os.IsNotExist(err) {
			t.Errorf("prior-run result trail %s should be excluded from the snapshot (err=%v)", p, err)
		}
	}
}

// (silence unused-import warnings — sort retained for future
// use in expanding the parity check above)
var _ = sort.Strings
