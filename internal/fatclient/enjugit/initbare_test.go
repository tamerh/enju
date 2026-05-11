package enjugit

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// TestInitBareWithSeedScaffoldsTemplatesDir verifies that a
// freshly-initialized bare repo carries enju/templates/.gitkeep
// in its initial commit. Without this scaffolding, a new
// project's enju_list_templates returns empty immediately after
// create_project — confusing "looks broken" UX.
func TestInitBareWithSeedScaffoldsTemplatesDir(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "seeded.git")
	if err := InitBareWithSeed(bareDir); err != nil {
		t.Fatalf("InitBareWithSeed: %v", err)
	}

	cloneDir := filepath.Join(t.TempDir(), "clone")
	gittest.CloneBranch(t, cloneDir, bareDir, "main")
	if _, err := os.Stat(filepath.Join(cloneDir, "README.md")); err != nil {
		t.Errorf("README.md missing from initial commit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cloneDir, "enju", "templates", ".gitkeep")); err != nil {
		t.Errorf("enju/templates/.gitkeep missing from initial commit: %v", err)
	}
}

func TestInitBareEmpty(t *testing.T) {
	bareDir := filepath.Join(t.TempDir(), "empty.git")
	if err := InitBareEmpty(bareDir); err != nil {
		t.Fatalf("InitBareEmpty: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bareDir, "HEAD")); err != nil {
		t.Errorf("bare HEAD missing: %v", err)
	}
	// Confirm git agrees this is a bare repo.
	out := gittest.Run(t, bareDir, "rev-parse", "--is-bare-repository")
	if out != "true" {
		t.Errorf("rev-parse --is-bare-repository = %q, want true", out)
	}
}

func TestPromoteWorkingTreeToBare(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	gittest.Init(t, wt)
	gittest.Commit(t, wt, "f.txt", "x", "init")

	bare := filepath.Join(t.TempDir(), "bare.git")
	if err := PromoteWorkingTreeToBare(wt, bare); err != nil {
		t.Fatalf("PromoteWorkingTreeToBare: %v", err)
	}
	// Bare exists.
	if _, err := os.Stat(filepath.Join(bare, "HEAD")); err != nil {
		t.Errorf("bare HEAD missing: %v", err)
	}
	// Idempotent — second call must not error.
	if err := PromoteWorkingTreeToBare(wt, bare); err != nil {
		t.Errorf("second call should be idempotent, got %v", err)
	}
	// Working tree's origin points at bare.
	got := gittest.Run(t, wt, "remote", "get-url", "origin")
	if got != bare {
		t.Errorf("origin URL: got %q, want %q", got, bare)
	}
}

// TestPromoteWorkingTreeToBare_TransfersAllBranches exercises the
// mirror-refspec path inside PromoteWorkingTreeToBare. The basic
// test above only seeds main, so it wouldn't catch a regression
// where the bare ends up HEAD-only. Bots fork topic branches off
// main → if the bare misses the operator's other branches,
// pushes from those branches break.
func TestPromoteWorkingTreeToBare_TransfersAllBranches(t *testing.T) {
	tmp := t.TempDir()
	wt := filepath.Join(tmp, "op-tree")
	bare := filepath.Join(tmp, "repos", "demo.git")

	gittest.Init(t, wt)
	gittest.Commit(t, wt, "README.md", "# op\n", "seed")
	headSHA := gittest.HeadSHA(t, wt)
	for _, name := range []string{"smoke-1", "smoke-2"} {
		gittest.CreateBranchAt(t, wt, name, headSHA)
	}

	if err := PromoteWorkingTreeToBare(wt, bare); err != nil {
		t.Fatalf("PromoteWorkingTreeToBare: %v", err)
	}

	// Bare is bare.
	isBare := gittest.Run(t, bare, "rev-parse", "--is-bare-repository")
	if isBare != "true" {
		t.Errorf("expected bare repo, got rev-parse --is-bare = %q", isBare)
	}
	// All three branches transferred.
	out := gittest.Run(t, bare, "for-each-ref", "--format=%(refname:short)", "refs/heads/")
	found := map[string]bool{"main": false, "smoke-1": false, "smoke-2": false}
	for _, line := range strings.Split(out, "\n") {
		line = strings.TrimSpace(line)
		if _, ok := found[line]; ok {
			found[line] = true
		}
	}
	for name, ok := range found {
		if !ok {
			t.Errorf("bare missing branch %q", name)
		}
	}
}

// TestPromoteWorkingTreeToBare_ReplacesExistingOrigin pins the
// defensive contract: an operator's adopted folder may already
// have an `origin` pointing at an old GitHub URL they're
// abandoning. Promoting must replace it, not silently leave the
// stale origin in place — otherwise future pushes route to the
// wrong remote.
func TestPromoteWorkingTreeToBare_ReplacesExistingOrigin(t *testing.T) {
	tmp := t.TempDir()
	wt := filepath.Join(tmp, "op-tree")
	bare := filepath.Join(tmp, "repos", "demo.git")

	gittest.Init(t, wt)
	gittest.Commit(t, wt, "README.md", "# op\n", "seed")
	gittest.AddRemote(t, wt, "origin", "https://example.com/old-remote.git")

	if err := PromoteWorkingTreeToBare(wt, bare); err != nil {
		t.Fatal(err)
	}

	got := gittest.Run(t, wt, "remote", "get-url", "origin")
	if got != bare {
		t.Errorf("expected origin replaced with %q, got %v", bare, got)
	}
}

// TestPromoteWorkingTreeToBare_RejectsNonGitDir verifies the input
// validation: pointing the promote at a directory that isn't a git
// working tree must return an error rather than silently corrupting
// the destination bare.
func TestPromoteWorkingTreeToBare_RejectsNonGitDir(t *testing.T) {
	tmp := t.TempDir()
	notARepo := filepath.Join(tmp, "plain-dir")
	_ = os.MkdirAll(notARepo, 0o755)
	bare := filepath.Join(tmp, "repos", "demo.git")

	if err := PromoteWorkingTreeToBare(notARepo, bare); err == nil {
		t.Fatal("expected error when source is not a git working tree")
	}
}

func TestIsLocalWorkingTree(t *testing.T) {
	tmp := t.TempDir()
	if IsLocalWorkingTree(tmp) {
		t.Error("plain dir without .git should not be a working tree")
	}
	if err := os.MkdirAll(filepath.Join(tmp, ".git"), 0755); err != nil {
		t.Fatal(err)
	}
	if !IsLocalWorkingTree(tmp) {
		t.Error("dir with .git/ should be detected as a working tree")
	}
	if IsLocalWorkingTree(filepath.Join(tmp, "nonexistent")) {
		t.Error("nonexistent path should not be a working tree")
	}
}
