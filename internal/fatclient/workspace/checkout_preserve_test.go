package workspace

// CheckoutBranch's interaction with untracked worktree files.
// The code comment on CheckoutBranch claims "Untracked files
// that the user authored (e.g. a enju.yaml pending
// auto-commit) are preserved by go-git's checkout regardless
// of Force." A tester reported an untracked
// enju/templates/async-b/ directory vanished after
// enju_create_run — if the claim is wrong, the fix is either
// to drop Force:true on new-branch checkouts, or to stash /
// restore untracked files around the checkout.
//
// These tests directly exercise that claim: write an
// untracked file, checkout a new branch, verify the file is
// still there.

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestCheckoutBranchPreservesUntrackedFiles is the direct
// repro for the "untracked files clobbered during
// enju_create_run" report. We simulate the user having
// authored a template file that isn't yet in git, then
// switching to a fresh new branch — go-git's Force:true in
// CheckoutBranch must not erase it.
func TestCheckoutBranchPreservesUntrackedFiles(t *testing.T) {
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

	// User authors an untracked template on main. This is
	// the tester's exact scenario — a directory under
	// enju/templates/ that hasn't been git-added yet.
	workDir := proj.WorkDir()
	untrackedDir := filepath.Join(workDir, "enju", "templates", "async-b")
	if err := os.MkdirAll(untrackedDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	untrackedFile := filepath.Join(untrackedDir, "enju.yaml")
	if err := os.WriteFile(untrackedFile, []byte("name: async-b\n"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Switch to a brand-new branch (triggers the Force:true
	// path in CheckoutBranch).
	proj.Lock()
	err = proj.CheckoutBranch("run-1")
	proj.Unlock()
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// The untracked file MUST still exist on disk.
	if _, err := os.Stat(untrackedFile); err != nil {
		t.Fatalf("untracked enju.yaml was clobbered after CheckoutBranch: %v", err)
	}
}

// TestCheckoutBranchRemovesTrackedFilesFromPriorBranch is the
// direct reproducer for the tester-reported "branch switching
// doesn't clean working tree — files from one run's branch
// leak onto another" bug. Scenario:
//
//  1. On main with tree = {README}.
//  2. Fork to branch-a, commit a new file scripts/a.sh on it.
//  3. Switch back to main (which doesn't track scripts/a.sh).
//  4. Expected: scripts/a.sh is removed from the worktree
//     (main doesn't have it).
//  5. Buggy: scripts/a.sh stays on disk, and the user sees
//     "main" with run-a's files leaked in.
//
// The existing-branch path of CheckoutBranch uses non-Force
// wt.Checkout. If gogit's non-Force checkout silently keeps
// files tracked on the prior branch but not on the target,
// the leak happens here.
func TestCheckoutBranchRemovesTrackedFilesFromPriorBranch(t *testing.T) {
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

	// Fork branch-a and commit a script there.
	proj.Lock()
	if err := proj.CheckoutBranch("branch-a"); err != nil {
		proj.Unlock()
		t.Fatalf("checkout branch-a: %v", err)
	}
	scriptPath := "scripts/a.sh"
	if _, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:seed",
		Username: "tester",
		Branch:   "branch-a",
		Files: []FileWrite{
			{RepoRelPath: scriptPath, Content: []byte("#!/bin/bash\necho a\n"), Mode: 0o755},
		},
		Trailers: EnjuTrailers{TaskID: "1:1:seed"},
	}); err != nil {
		proj.Unlock()
		t.Fatalf("seed branch-a commit: %v", err)
	}
	proj.Unlock()

	// Switch back to main. Main doesn't track scripts/a.sh,
	// so it MUST be removed from the worktree.
	proj.Lock()
	err = proj.CheckoutBranch("main")
	proj.Unlock()
	if err != nil {
		t.Fatalf("checkout main: %v", err)
	}

	full := filepath.Join(proj.WorkDir(), scriptPath)
	if _, err := os.Stat(full); err == nil {
		t.Fatalf("scripts/a.sh still on disk after switching main — branch-a's tracked files leaked onto main's worktree")
	}
}

// TestCheckoutBranchMultiBranchCleanSwitches simulates the
// tester's scenario more directly: two run branches each
// with their own committed files, and the user bouncing
// between them + main. After every switch the worktree
// MUST reflect only the target branch's tracked content —
// no leakage of files tracked elsewhere. Catches a class
// of bug where gogit's non-Force wt.Checkout on an
// existing branch might silently keep "stale tracked"
// files that should be wiped.
func TestCheckoutBranchMultiBranchCleanSwitches(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	// Commit file-a on branch-a.
	proj.Lock()
	proj.CheckoutBranch("branch-a")
	_, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:a",
		Username: "t",
		Branch:   "branch-a",
		Files:    []FileWrite{{RepoRelPath: "file-a.txt", Content: []byte("A")}},
		Trailers: EnjuTrailers{TaskID: "1:1:a"},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("seed a: %v", err)
	}

	// Commit file-b on branch-b (forked from main, so
	// doesn't inherit branch-a's file).
	proj.Lock()
	proj.CheckoutBranch("branch-b")
	_, err = proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:b",
		Username: "t",
		Branch:   "branch-b",
		Files:    []FileWrite{{RepoRelPath: "file-b.txt", Content: []byte("B")}},
		Trailers: EnjuTrailers{TaskID: "1:1:b"},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("seed b: %v", err)
	}

	// Now bounce: switch back to branch-a. Worktree should
	// have file-a.txt but NOT file-b.txt.
	proj.Lock()
	err = proj.CheckoutBranch("branch-a")
	proj.Unlock()
	if err != nil {
		t.Fatalf("re-checkout a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj.WorkDir(), "file-a.txt")); err != nil {
		t.Errorf("file-a.txt missing on branch-a: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj.WorkDir(), "file-b.txt")); err == nil {
		t.Errorf("file-b.txt leaked onto branch-a worktree")
	}

	// Switch to main. Should have neither.
	proj.Lock()
	err = proj.CheckoutBranch("main")
	proj.Unlock()
	if err != nil {
		t.Fatalf("checkout main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj.WorkDir(), "file-a.txt")); err == nil {
		t.Errorf("file-a.txt leaked onto main worktree")
	}
	if _, err := os.Stat(filepath.Join(proj.WorkDir(), "file-b.txt")); err == nil {
		t.Errorf("file-b.txt leaked onto main worktree")
	}

	// Switch to branch-b. Should have file-b, not file-a.
	proj.Lock()
	err = proj.CheckoutBranch("branch-b")
	proj.Unlock()
	if err != nil {
		t.Fatalf("re-checkout b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj.WorkDir(), "file-b.txt")); err != nil {
		t.Errorf("file-b.txt missing on branch-b: %v", err)
	}
	if _, err := os.Stat(filepath.Join(proj.WorkDir(), "file-a.txt")); err == nil {
		t.Errorf("file-a.txt leaked onto branch-b worktree")
	}
}

// TestCheckoutBranchPriorBranchFilesNotOnMainAfterEnsureBundle
// replicates the scenario most likely to match the tester's
// "pollution of main" report: two run branches with their own
// template bundles, alternating create_run() calls that each
// trigger EnsureBundleOnDefault (which switches to main,
// commits the bundle, then leaves). Verifies main's worktree
// doesn't accumulate files that belong to the OTHER run
// branch's snapshot.
func TestCheckoutBranchPriorBranchFilesNotOnMainAfterEnsureBundle(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(1, bare)

	// Simulate create_run(bundle-a, branch=run-a):
	//   1. EnsureBundleOnDefault commits bundle-a to main.
	//   2. CheckoutBranch(run-a) forks from main-with-bundle-a.
	//   3. Snapshot commit on run-a with enju/runs/1/template-snapshot/
	workDir := proj.WorkDir()
	// Author bundle-a files untracked on main first.
	if err := os.MkdirAll(filepath.Join(workDir, "enju/templates/bundle-a"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "enju/templates/bundle-a/enju.yaml"), []byte("name: a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	proj.Lock()
	if _, err := proj.EnsureBundleOnDefault("enju/templates/bundle-a", "t", "t@x", ""); err != nil {
		proj.Unlock()
		t.Fatalf("ensure a: %v", err)
	}
	// Branch off to run-a.
	if err := proj.CheckoutBranch("run-a"); err != nil {
		proj.Unlock()
		t.Fatal(err)
	}
	// Snapshot commit on run-a.
	snapFiles, err := proj.ReadBundleFiles("enju/templates/bundle-a", "enju/runs/1/template")
	if err != nil {
		proj.Unlock()
		t.Fatalf("read bundle a: %v", err)
	}
	if _, err := proj.CommitFiles(CommitFilesRequest{
		Files:       snapFiles,
		CommitMsg:   "snapshot run-a",
		AuthorName:  "t",
		AuthorEmail: "t@x",
		Branch:      "run-a",
	}); err != nil {
		proj.Unlock()
		t.Fatalf("snapshot a: %v", err)
	}
	proj.Unlock()

	// Second create_run on a different template.
	if err := os.MkdirAll(filepath.Join(workDir, "enju/templates/bundle-b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "enju/templates/bundle-b/enju.yaml"), []byte("name: b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	proj.Lock()
	if _, err := proj.EnsureBundleOnDefault("enju/templates/bundle-b", "t", "t@x", ""); err != nil {
		proj.Unlock()
		t.Fatalf("ensure b: %v", err)
	}
	if err := proj.CheckoutBranch("run-b"); err != nil {
		proj.Unlock()
		t.Fatal(err)
	}
	snapFilesB, err := proj.ReadBundleFiles("enju/templates/bundle-b", "enju/runs/2/template")
	if err != nil {
		proj.Unlock()
		t.Fatalf("read bundle b: %v", err)
	}
	if _, err := proj.CommitFiles(CommitFilesRequest{
		Files:       snapFilesB,
		CommitMsg:   "snapshot run-b",
		AuthorName:  "t",
		AuthorEmail: "t@x",
		Branch:      "run-b",
	}); err != nil {
		proj.Unlock()
		t.Fatalf("snapshot b: %v", err)
	}
	proj.Unlock()

	// Switch to main. Main should have bundle-a, bundle-b
	// (both auto-committed), but NO snapshot files from
	// either run (those land on the respective run branches).
	proj.Lock()
	if err := proj.CheckoutBranch("main"); err != nil {
		proj.Unlock()
		t.Fatalf("back to main: %v", err)
	}
	proj.Unlock()

	// Positive: bundle-a and bundle-b enju.yaml are on main.
	for _, path := range []string{"enju/templates/bundle-a/enju.yaml", "enju/templates/bundle-b/enju.yaml"} {
		if _, err := os.Stat(filepath.Join(workDir, path)); err != nil {
			t.Errorf("main missing expected template %s: %v", path, err)
		}
	}
	// Negative: run-a and run-b snapshots are NOT on main's worktree.
	// These files are only committed on run-a / run-b branches.
	for _, path := range []string{
		"enju/runs/1/template-snapshot/enju.yaml",
		"enju/runs/2/template-snapshot/enju.yaml",
	} {
		if _, err := os.Stat(filepath.Join(workDir, path)); err == nil {
			t.Errorf("main worktree polluted with run-branch snapshot: %s", path)
		}
	}
}

// TestCheckoutBranchPreservesUntrackedRoot exercises an
// untracked file at the workspace root (not nested under
// enju/templates/), in case gogit's Force behaviour differs
// by path depth or match against tracked paths.
func TestCheckoutBranchPreservesUntrackedRoot(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, _ := ws.ForProject(2, bare)

	workDir := proj.WorkDir()
	untracked := filepath.Join(workDir, "my-scratch.txt")
	if err := os.WriteFile(untracked, []byte("wip"), 0644); err != nil {
		t.Fatalf("write: %v", err)
	}

	proj.Lock()
	err := proj.CheckoutBranch("lane-42")
	proj.Unlock()
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	if _, err := os.Stat(untracked); err != nil {
		t.Fatalf("untracked root file was clobbered: %v", err)
	}
}

// TestCheckoutBranchPreservesGitignoredFiles is the regression
// for the "secrets dir wiped during create_run" report. A user
// placed credential files under .secrets/ (gitignored), and a
// subsequent create_run's Force checkout wiped them. Root
// cause: the old snapshotUntrackedFiles used go-git's Status,
// which filters ignored paths out of the Untracked set. Fix:
// walk the filesystem and snapshot everything git isn't
// tracking — including gitignored paths.
func TestCheckoutBranchPreservesGitignoredFiles(t *testing.T) {
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

	// Commit a .gitignore that excludes .secrets/ on main, so
	// when we drop files there they're ignored rather than
	// untracked.
	workDir := proj.WorkDir()
	proj.Lock()
	if _, err := proj.CommitFiles(CommitFilesRequest{
		Files: []FileWrite{
			{RepoRelPath: ".gitignore", Content: []byte(".secrets/\n"), Mode: 0o644},
		},
		CommitMsg:   "seed gitignore",
		AuthorName:  "t",
		AuthorEmail: "t@x",
	}); err != nil {
		proj.Unlock()
		t.Fatalf("seed gitignore: %v", err)
	}
	proj.Unlock()

	// Drop a gitignored secret file. Status() on this path
	// returns no entry (go-git filters ignored), so the old
	// snapshot logic missed it entirely.
	secretsDir := filepath.Join(workDir, ".secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatalf("mkdir .secrets: %v", err)
	}
	secretPath := filepath.Join(secretsDir, "openrouter")
	secretBody := []byte("OPENROUTER_KEY=sk-test-value\n")
	if err := os.WriteFile(secretPath, secretBody, 0o600); err != nil {
		t.Fatalf("write secret: %v", err)
	}

	// Simulate what create_run does — switch to a brand-new
	// branch (Force checkout path).
	proj.Lock()
	err = proj.CheckoutBranch("run-1")
	proj.Unlock()
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// The secret MUST still exist on disk, byte-identical.
	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("gitignored secret wiped after CheckoutBranch: %v", err)
	}
	if string(got) != string(secretBody) {
		t.Errorf("secret content changed: got %q, want %q", got, secretBody)
	}
	// And the restricted mode should survive the round trip —
	// credentials at 0644 would be a regression.
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatalf("stat secret: %v", err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret mode changed: got %o, want %o", info.Mode().Perm(), 0o600)
	}
}

// TestCheckoutBranchRenamePreserveLargeFileConstantMemory is
// the safety test for the bio-workload case: a multi-GB
// gitignored file must NOT be read into memory during branch
// switch. We can't easily assert "didn't allocate" in a unit
// test, but we CAN assert the file survives a checkout with
// only microseconds of wall time — infeasible if the preserve
// logic were still reading bytes.
//
// We use a 128 MiB sparse file (enough to make a read-based
// approach measurable) and expect the whole CheckoutBranch
// call to finish in under 500 ms on any reasonable host. A
// read-based implementation would take seconds.
func TestCheckoutBranchRenamePreserveLargeFileConstantMemory(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Commit a .gitignore so data/ is ignored rather than
	// plain-untracked (exercises the ignored-path codepath
	// specifically).
	proj.Lock()
	if _, err := proj.CommitFiles(CommitFilesRequest{
		Files:       []FileWrite{{RepoRelPath: ".gitignore", Content: []byte("data/\n"), Mode: 0o644}},
		CommitMsg:   "seed gitignore",
		AuthorName:  "t",
		AuthorEmail: "t@x",
	}); err != nil {
		proj.Unlock()
		t.Fatalf("seed gitignore: %v", err)
	}
	proj.Unlock()

	// Create a 128 MiB sparse file under the gitignored dir.
	// Sparse = Seek+Write(1 byte at offset N) — costs almost
	// nothing on disk but has the reported size, which is
	// what the buggy memory-based approach would try to read.
	workDir := proj.WorkDir()
	dataDir := filepath.Join(workDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatalf("mkdir data: %v", err)
	}
	bigPath := filepath.Join(dataDir, "huge.bin")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatalf("create big file: %v", err)
	}
	const size int64 = 128 * 1024 * 1024
	if _, err := f.Seek(size-1, 0); err != nil {
		t.Fatalf("seek: %v", err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatalf("write sentinel: %v", err)
	}
	f.Close()

	// Time the checkout.
	start := time.Now()
	proj.Lock()
	err = proj.CheckoutBranch("bio-run-1")
	proj.Unlock()
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// The file must still be there, still 128 MiB.
	info, err := os.Stat(bigPath)
	if err != nil {
		t.Fatalf("big file missing after checkout: %v", err)
	}
	if info.Size() != size {
		t.Errorf("big file size changed: got %d, want %d", info.Size(), size)
	}

	// Rename-based preservation should finish in well under
	// 500ms. A memory-based approach at 128MiB would take
	// noticeably longer (read + write = 256MiB of I/O minimum).
	if elapsed > 500*time.Millisecond {
		t.Errorf("checkout took %v on a 128MiB sparse file — likely reading bytes into memory, not renaming", elapsed)
	}
}

// TestRestoreFromPreserveLeavesConflictingCopy is a unit test
// for the conflict-handling branch of restoreFromPreserve:
// when the post-checkout workDir already has a file at the
// target path (branch tracks it), the preserved copy must
// stay in the preserve dir rather than overwriting branch
// content. Exercised directly — the natural end-to-end
// scenario (new-branch Force checkout where the new branch
// also tracks a path the user had untracked) is rare because
// branchBaseHash forks from origin/main, which by construction
// doesn't track paths the user has untracked.
func TestRestoreFromPreserveLeavesConflictingCopy(t *testing.T) {
	workDir := t.TempDir()
	preserveDir := workDir + ".preserve-in-progress"

	// Preserve dir has the user's version.
	if err := os.MkdirAll(preserveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("user version\n")
	if err := os.WriteFile(filepath.Join(preserveDir, "notes.md"), userContent, 0o644); err != nil {
		t.Fatal(err)
	}
	// workDir has the branch's version (simulating post-checkout).
	branchContent := []byte("branch version\n")
	if err := os.WriteFile(filepath.Join(workDir, "notes.md"), branchContent, 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := []preservedEntry{{relPath: "notes.md"}}
	conflicts, err := restoreFromPreserve(workDir, preserveDir, manifest)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(conflicts) != 1 || conflicts[0].relPath != "notes.md" {
		t.Errorf("expected notes.md in conflicts, got %+v", conflicts)
	}
	if n := countConflictFiles(preserveDir, conflicts); n != 1 {
		t.Errorf("expected 1 conflicting file, got %d", n)
	}

	// workDir keeps branch version.
	got, err := os.ReadFile(filepath.Join(workDir, "notes.md"))
	if err != nil || string(got) != string(branchContent) {
		t.Errorf("workDir clobbered: got %q err=%v", got, err)
	}
	// Preserve dir keeps user version for manual review.
	preserved, err := os.ReadFile(filepath.Join(preserveDir, "notes.md"))
	if err != nil || string(preserved) != string(userContent) {
		t.Errorf("preserved copy lost: got %q err=%v", preserved, err)
	}
}

// TestCheckoutBranchSubtreeRenameIsWholeDir: when a whole
// directory has no tracked descendants, we rename the dir
// itself (single syscall) rather than walking in and renaming
// files one-by-one. Hard to observe directly, but we can
// check that a deeply-nested untracked tree round-trips
// intact, including an empty leaf directory (which memory
// preservation would have silently dropped).
func TestCheckoutBranchSubtreeRenameIsWholeDir(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewWorkspace(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Nested untracked tree with a file, a subdir with a file,
	// and an empty subdir.
	workDir := proj.WorkDir()
	for _, rel := range []string{"scratch/a.txt", "scratch/nested/b.txt"} {
		full := filepath.Join(workDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	emptyDir := filepath.Join(workDir, "scratch", "empty")
	if err := os.MkdirAll(emptyDir, 0o755); err != nil {
		t.Fatal(err)
	}

	proj.Lock()
	err = proj.CheckoutBranch("new-branch")
	proj.Unlock()
	if err != nil {
		t.Fatalf("checkout: %v", err)
	}

	// All three paths must be present.
	for _, rel := range []string{"scratch/a.txt", "scratch/nested/b.txt"} {
		got, err := os.ReadFile(filepath.Join(workDir, rel))
		if err != nil {
			t.Errorf("%s missing after checkout: %v", rel, err)
			continue
		}
		if string(got) != rel {
			t.Errorf("%s content changed: got %q", rel, got)
		}
	}
	// The empty dir must still exist (wholesale rename
	// preserves it; per-file snapshot would have lost it).
	if info, err := os.Stat(emptyDir); err != nil || !info.IsDir() {
		t.Errorf("empty dir scratch/empty did not survive checkout: %v", err)
	}
}

// TestCrashRecoveryOfLeftoverPreserveDir: simulate a process
// crash mid-preservation by manually leaving a preserve dir
// beside the workDir with some files in it. Next ForProject
// call should drain the preserve dir back into the workspace.
func TestCrashRecoveryOfLeftoverPreserveDir(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	rootDir := t.TempDir()
	ws, _ := NewWorkspace(rootDir, nullLogger())
	proj, err := ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("first clone: %v", err)
	}
	workDir := proj.WorkDir()

	// Manually populate a preserve dir to simulate the crashed
	// state. Include one dir with a file (typical post-crash
	// layout) and one file in a conflict path (notes.md — we'll
	// also drop a workDir/notes.md to force a conflict).
	preserveDir := workDir + ".preserve-in-progress"
	if err := os.MkdirAll(filepath.Join(preserveDir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preserveDir, "scratch", "keeper.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preserveDir, "conflict.txt"), []byte("preserved version"), 0o644); err != nil {
		t.Fatal(err)
	}
	// workDir already has a conflict.txt (simulating the
	// branch tracks it).
	if err := os.WriteFile(filepath.Join(workDir, "conflict.txt"), []byte("branch version"), 0o644); err != nil {
		t.Fatal(err)
	}

	// Drop the cached Project handle so ForProject goes
	// through openOrClone again (which invokes recovery).
	proj.Lock()
	proj.Unlock()
	ws.clients = map[int64]*Project{}

	// Re-open: recovery should run.
	_, err = ws.ForProject(1, bare)
	if err != nil {
		t.Fatalf("re-open: %v", err)
	}

	// keeper.txt should have been restored.
	got, err := os.ReadFile(filepath.Join(workDir, "scratch", "keeper.txt"))
	if err != nil {
		t.Errorf("keeper.txt not restored: %v", err)
	} else if string(got) != "kept" {
		t.Errorf("keeper.txt content wrong: %q", got)
	}

	// conflict.txt in workDir should still be the branch
	// version (recovery must not clobber it).
	got, err = os.ReadFile(filepath.Join(workDir, "conflict.txt"))
	if err != nil || string(got) != "branch version" {
		t.Errorf("conflict.txt clobbered: got=%q err=%v", got, err)
	}
	// And the preserved conflict.txt should still be in the
	// preserve dir — recovery leaves conflicts alone.
	got, err = os.ReadFile(filepath.Join(preserveDir, "conflict.txt"))
	if err != nil || string(got) != "preserved version" {
		t.Errorf("preserve copy of conflict.txt gone: got=%q err=%v", got, err)
	}
}

// TestCountConflictFilesDescendsIntoDirs: a wholesale-dir
// conflict must report every file it strands, not just "1
// conflict entry." Otherwise a gitignored data/ dir holding
// 50 files that got stranded by a branch that happens to
// track data/ would log "conflict_count: 1" and badly
// understate the divergence.
func TestCountConflictFilesDescendsIntoDirs(t *testing.T) {
	preserveDir := t.TempDir()
	// Build a preserved subtree at data/ with 3 files across 2 levels.
	for _, rel := range []string{"data/a.txt", "data/b.txt", "data/nested/c.txt"} {
		full := filepath.Join(preserveDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Also add a single-file conflict.
	if err := os.WriteFile(filepath.Join(preserveDir, "note.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	conflicts := []preservedEntry{
		{relPath: "data", isDir: true},
		{relPath: "note.md", isDir: false},
	}
	got := countConflictFiles(preserveDir, conflicts)
	want := 4 // 3 files under data/ + note.md
	if got != want {
		t.Errorf("countConflictFiles: got %d, want %d (dir-entry conflict should count descendants)", got, want)
	}
}
