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
