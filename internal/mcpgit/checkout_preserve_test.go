package mcpgit

// CheckoutBranch's interaction with untracked worktree files.
// The code comment on CheckoutBranch claims "Untracked files
// that the user authored (e.g. a template.yaml pending
// auto-commit) are preserved by go-git's checkout regardless
// of Force." A tester reported an untracked
// enju_templates/async-b/ directory vanished after
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
	// enju_templates/ that hasn't been git-added yet.
	workDir := proj.WorkDir()
	untrackedDir := filepath.Join(workDir, "enju_templates", "async-b")
	if err := os.MkdirAll(untrackedDir, 0755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	untrackedFile := filepath.Join(untrackedDir, "template.yaml")
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
		t.Fatalf("untracked template.yaml was clobbered after CheckoutBranch: %v", err)
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
	//   3. Snapshot commit on run-a with .enju/runs/1/template/
	workDir := proj.WorkDir()
	// Author bundle-a files untracked on main first.
	if err := os.MkdirAll(filepath.Join(workDir, "enju_templates/bundle-a"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "enju_templates/bundle-a/template.yaml"), []byte("name: a\n"), 0644); err != nil {
		t.Fatal(err)
	}
	proj.Lock()
	if _, err := proj.EnsureBundleOnDefault("enju_templates/bundle-a", "t", "t@x", ""); err != nil {
		proj.Unlock()
		t.Fatalf("ensure a: %v", err)
	}
	// Branch off to run-a.
	if err := proj.CheckoutBranch("run-a"); err != nil {
		proj.Unlock()
		t.Fatal(err)
	}
	// Snapshot commit on run-a.
	snapFiles, err := proj.ReadBundleFiles("enju_templates/bundle-a", ".enju/runs/1/template")
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
	if err := os.MkdirAll(filepath.Join(workDir, "enju_templates/bundle-b"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "enju_templates/bundle-b/template.yaml"), []byte("name: b\n"), 0644); err != nil {
		t.Fatal(err)
	}
	proj.Lock()
	if _, err := proj.EnsureBundleOnDefault("enju_templates/bundle-b", "t", "t@x", ""); err != nil {
		proj.Unlock()
		t.Fatalf("ensure b: %v", err)
	}
	if err := proj.CheckoutBranch("run-b"); err != nil {
		proj.Unlock()
		t.Fatal(err)
	}
	snapFilesB, err := proj.ReadBundleFiles("enju_templates/bundle-b", ".enju/runs/2/template")
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

	// Positive: bundle-a and bundle-b template.yaml are on main.
	for _, path := range []string{"enju_templates/bundle-a/template.yaml", "enju_templates/bundle-b/template.yaml"} {
		if _, err := os.Stat(filepath.Join(workDir, path)); err != nil {
			t.Errorf("main missing expected template %s: %v", path, err)
		}
	}
	// Negative: run-a and run-b snapshots are NOT on main's worktree.
	// These files are only committed on run-a / run-b branches.
	for _, path := range []string{
		".enju/runs/1/template/template.yaml",
		".enju/runs/2/template/template.yaml",
	} {
		if _, err := os.Stat(filepath.Join(workDir, path)); err == nil {
			t.Errorf("main worktree polluted with run-branch snapshot: %s", path)
		}
	}
}

// TestCheckoutBranchPreservesUntrackedRoot exercises an
// untracked file at the workspace root (not nested under
// enju_templates/), in case gogit's Force behaviour differs
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
