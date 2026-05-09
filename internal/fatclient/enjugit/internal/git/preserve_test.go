package git

// Tests for the rename-based preserve mechanism around Force
// checkouts. See preserve.go for the design rationale + invariants.
// TestCheckout_PreservesUntracked in branch_test.go pins the basic
// untracked-survives case; this file covers the load-bearing edges:
// gitignored files, leftover-dir recovery, conflict handling,
// large-file constant memory.

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestCheckout_PreservesGitignoredFiles is the regression for the
// ".secrets/ wiped during create_run" report. A user placed
// credential files under a gitignored .secrets/ dir, and a
// subsequent Force checkout wiped them. Root cause: the original
// snapshot used go-git's Status, which filters ignored paths out
// of the Untracked set. Fix: the rename walk treats anything not
// in the index as preservable, including gitignored paths.
func TestCheckout_PreservesGitignoredFiles(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	// Commit a .gitignore so .secrets/ is ignored rather than
	// plain-untracked (drives the ignored-path codepath).
	if _, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: ".gitignore", Content: []byte(".secrets/\n")},
		},
		Message: "seed gitignore",
	}); err != nil {
		t.Fatalf("seed gitignore: %v", err)
	}

	// Drop a gitignored secret with restrictive mode.
	secretsDir := filepath.Join(c.workDir, ".secrets")
	if err := os.MkdirAll(secretsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	secretPath := filepath.Join(secretsDir, "openrouter")
	secretBody := []byte("OPENROUTER_KEY=sk-test-value\n")
	if err := os.WriteFile(secretPath, secretBody, 0o600); err != nil {
		t.Fatal(err)
	}

	// Brand-new branch → Force checkout → preserve dance runs.
	headSHA, _, _ := c.Head()
	if err := c.CreateBranchAt("run-1", headSHA); err != nil {
		t.Fatal(err)
	}
	if err := c.Checkout("run-1"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	got, err := os.ReadFile(secretPath)
	if err != nil {
		t.Fatalf("gitignored secret wiped after Checkout: %v", err)
	}
	if string(got) != string(secretBody) {
		t.Errorf("secret content changed: got %q, want %q", got, secretBody)
	}
	info, err := os.Stat(secretPath)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Errorf("secret mode changed: got %o, want %o", info.Mode().Perm(), 0o600)
	}
}

// TestRestoreFromPreserve_LeavesConflictingCopy is the unit test
// for restoreFromPreserve's conflict branch: when the post-checkout
// workDir already has a file at the target path (the new branch
// tracks it), the preserved copy must stay in the preserve dir
// rather than overwrite branch content.
func TestRestoreFromPreserve_LeavesConflictingCopy(t *testing.T) {
	workDir := t.TempDir()
	preserveDir := workDir + PreserveDirSuffix

	if err := os.MkdirAll(preserveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	userContent := []byte("user version\n")
	if err := os.WriteFile(filepath.Join(preserveDir, "notes.md"), userContent, 0o644); err != nil {
		t.Fatal(err)
	}
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
	got, _ := os.ReadFile(filepath.Join(workDir, "notes.md"))
	if string(got) != string(branchContent) {
		t.Errorf("workDir clobbered: got %q", got)
	}
	preserved, _ := os.ReadFile(filepath.Join(preserveDir, "notes.md"))
	if string(preserved) != string(userContent) {
		t.Errorf("preserved copy lost: got %q", preserved)
	}
}

// TestCheckout_PreservesNestedNonTracked covers the per-file
// preserve walk: nested untracked files survive a Force checkout,
// each renamed individually since the wholesale-subtree shortcut
// is gone. (Empty leaf directories are not preserved — git
// doesn't track them.)
func TestCheckout_PreservesNestedNonTracked(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	for _, rel := range []string{"scratch/a.txt", "scratch/nested/b.txt"} {
		full := filepath.Join(c.workDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(rel), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("new-branch", headSHA)
	if err := c.Checkout("new-branch"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	for _, rel := range []string{"scratch/a.txt", "scratch/nested/b.txt"} {
		got, err := os.ReadFile(filepath.Join(c.workDir, rel))
		if err != nil {
			t.Errorf("%s missing: %v", rel, err)
			continue
		}
		if string(got) != rel {
			t.Errorf("%s content changed: got %q", rel, got)
		}
	}
}

// TestRestoreFromPreserve_DropsByteEqualConflict pins the
// no-phantom-conflicts rule: when restore finds the target
// path already on disk AND its bytes equal the preserved
// copy's bytes, the preserved copy is silently removed
// rather than being flagged as a conflict. This is the
// "branch tree just rewrote what we tried to preserve"
// case (typical right after a successful submit), where
// the older code accumulated stale entries in the preserve
// dir across iterations.
func TestRestoreFromPreserve_DropsByteEqualConflict(t *testing.T) {
	workDir := t.TempDir()
	preserveDir := workDir + PreserveDirSuffix
	if err := os.MkdirAll(preserveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	body := []byte("identical bytes\n")
	if err := os.WriteFile(filepath.Join(preserveDir, "src/foo.txt"), body, 0o644); err != nil {
		// preserve subdir doesn't exist yet; create it.
		if err := os.MkdirAll(filepath.Join(preserveDir, "src"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(preserveDir, "src/foo.txt"), body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(workDir, "src"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "src/foo.txt"), body, 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := []preservedEntry{{relPath: "src/foo.txt"}}
	conflicts, err := restoreFromPreserve(workDir, preserveDir, manifest)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(conflicts) != 0 {
		t.Errorf("expected no conflicts when bytes match, got %+v", conflicts)
	}
	if _, err := os.Stat(filepath.Join(preserveDir, "src/foo.txt")); err == nil {
		t.Errorf("preserved copy should have been removed when bytes match")
	}
	got, _ := os.ReadFile(filepath.Join(workDir, "src/foo.txt"))
	if string(got) != string(body) {
		t.Errorf("workdir content changed: got %q", got)
	}
}

// TestRestoreFromPreserve_KeepsDifferingConflict pins the
// other half: when bytes differ, the preserved copy stays in
// the preserve dir for manual review — that's a real divergence
// the user needs to see (untracked work that collides with
// branch-tracked content).
func TestRestoreFromPreserve_KeepsDifferingConflict(t *testing.T) {
	workDir := t.TempDir()
	preserveDir := workDir + PreserveDirSuffix
	if err := os.MkdirAll(preserveDir, 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(filepath.Join(preserveDir, "notes.md"), []byte("user version"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(workDir, "notes.md"), []byte("branch version"), 0o644); err != nil {
		t.Fatal(err)
	}

	manifest := []preservedEntry{{relPath: "notes.md"}}
	conflicts, err := restoreFromPreserve(workDir, preserveDir, manifest)
	if err != nil {
		t.Fatalf("restore: %v", err)
	}
	if len(conflicts) != 1 {
		t.Errorf("expected 1 conflict on differing bytes, got %d", len(conflicts))
	}
	got, _ := os.ReadFile(filepath.Join(preserveDir, "notes.md"))
	if string(got) != "user version" {
		t.Errorf("preserved copy lost: %q", got)
	}
}

// TestRecoverLeftoverPreserve_RestoresAndLeavesConflicts simulates
// a process crash mid-preservation: a leftover preserve dir
// beside workDir with mixed content. RecoverLeftoverPreserve
// must drain non-conflicting entries back into workDir and leave
// conflicting entries in the preserve dir for manual review.
func TestRecoverLeftoverPreserve_RestoresAndLeavesConflicts(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	preserveDir := c.workDir + PreserveDirSuffix
	if err := os.MkdirAll(filepath.Join(preserveDir, "scratch"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preserveDir, "scratch", "keeper.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(preserveDir, "conflict.txt"), []byte("preserved version"), 0o644); err != nil {
		t.Fatal(err)
	}
	// workDir has a conflict.txt already (simulating the branch
	// tracks it post-checkout).
	if err := os.WriteFile(filepath.Join(c.workDir, "conflict.txt"), []byte("branch version"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := RecoverLeftoverPreserve(c.workDir, c.logger); err != nil {
		t.Fatalf("RecoverLeftoverPreserve: %v", err)
	}

	// Non-conflicting file restored.
	got, err := os.ReadFile(filepath.Join(c.workDir, "scratch", "keeper.txt"))
	if err != nil {
		t.Errorf("keeper.txt not restored: %v", err)
	} else if string(got) != "kept" {
		t.Errorf("keeper.txt content wrong: %q", got)
	}
	// Conflicting workDir file untouched.
	got, err = os.ReadFile(filepath.Join(c.workDir, "conflict.txt"))
	if err != nil || string(got) != "branch version" {
		t.Errorf("conflict.txt clobbered: got=%q err=%v", got, err)
	}
	// Preserved conflicting copy still in preserve dir for review.
	got, err = os.ReadFile(filepath.Join(preserveDir, "conflict.txt"))
	if err != nil || string(got) != "preserved version" {
		t.Errorf("preserve copy of conflict.txt gone: got=%q err=%v", got, err)
	}
}

// TestCheckout_DrainsLeftoverPreserveDir: a preserve dir from a
// prior crashed checkout (still on disk) gets drained at the top
// of the next Checkout. Without the drain,
// movePreserveNonTracked would rename fresh paths INTO the dirty
// dir and produce undefined state.
func TestCheckout_DrainsLeftoverPreserveDir(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	preserveDir := c.workDir + PreserveDirSuffix
	if err := os.MkdirAll(preserveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	preservedContent := []byte("recovered from prior crash\n")
	if err := os.WriteFile(filepath.Join(preserveDir, "leftover.md"), preservedContent, 0o644); err != nil {
		t.Fatal(err)
	}

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("feature-x", headSHA)
	if err := c.Checkout("feature-x"); err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	got, err := os.ReadFile(filepath.Join(c.workDir, "leftover.md"))
	if err != nil {
		t.Fatalf("leftover.md not restored: %v", err)
	}
	if string(got) != string(preservedContent) {
		t.Errorf("restored content mismatch: got %q", got)
	}
	if _, err := os.Stat(preserveDir); err == nil {
		t.Error("preserve dir should be removed after successful drain")
	}
}

// TestCheckout_RefusesWhenLeftoverConflicts pins the safety guard:
// when the leftover preserve dir contains a file the current
// branch ALSO tracks, drain leaves it for manual review and the
// new checkout returns ErrPreserveDirCollision rather than
// proceeding. Fresh preservation must never write into a dirty dir.
func TestCheckout_RefusesWhenLeftoverConflicts(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	preserveDir := c.workDir + PreserveDirSuffix
	if err := os.MkdirAll(preserveDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// README.md is in the seeded initial commit on main.
	if err := os.WriteFile(filepath.Join(preserveDir, "README.md"), []byte("conflicting"), 0o644); err != nil {
		t.Fatal(err)
	}

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("feature-y", headSHA)
	err := c.Checkout("feature-y")
	if err == nil {
		t.Fatal("expected error refusing checkout, got nil")
	}
	if !errors.Is(err, ErrPreserveDirCollision) {
		t.Errorf("expected ErrPreserveDirCollision, got %v", err)
	}
	// Conflicting preserve file untouched — operator can review.
	if _, err := os.Stat(filepath.Join(preserveDir, "README.md")); err != nil {
		t.Errorf("conflicting preserve file should still be present: %v", err)
	}
	_ = strings.Contains // silence unused; future helper expansion
}

// TestCheckout_PreservesLargeFileConstantMemory is the safety
// test for the bio-workload case: a multi-GB gitignored file
// must NOT be read into memory during branch switch. We use a
// 128 MiB sparse file and assert the whole Checkout finishes in
// well under 500 ms — infeasible if the preserve logic were
// reading bytes. Rename-based preservation runs in O(1) syscalls
// independent of file size.
func TestCheckout_PreservesLargeFileConstantMemory(t *testing.T) {
	bare := initBareRemote(t)
	seedBareWithInitialCommit(t, bare)
	c := freshClone(t, bare)

	if _, err := c.CommitFiles(CommitRequest{
		Files: []FileWrite{
			{RepoRelPath: ".gitignore", Content: []byte("data/\n")},
		},
		Message: "seed gitignore",
	}); err != nil {
		t.Fatalf("seed gitignore: %v", err)
	}

	dataDir := filepath.Join(c.workDir, "data")
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		t.Fatal(err)
	}
	bigPath := filepath.Join(dataDir, "huge.bin")
	f, err := os.Create(bigPath)
	if err != nil {
		t.Fatal(err)
	}
	const size int64 = 128 * 1024 * 1024
	if _, err := f.Seek(size-1, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Write([]byte{0}); err != nil {
		t.Fatal(err)
	}
	f.Close()

	headSHA, _, _ := c.Head()
	c.CreateBranchAt("bio-run-1", headSHA)
	start := time.Now()
	err = c.Checkout("bio-run-1")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("Checkout: %v", err)
	}

	info, err := os.Stat(bigPath)
	if err != nil {
		t.Fatalf("big file missing after checkout: %v", err)
	}
	if info.Size() != size {
		t.Errorf("big file size changed: got %d, want %d", info.Size(), size)
	}
	if elapsed > 500*time.Millisecond {
		t.Errorf("checkout took %v on a 128MiB sparse file — likely reading bytes, not renaming", elapsed)
	}
}
