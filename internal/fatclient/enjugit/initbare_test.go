package enjugit

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
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
	if _, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{
		URL:           bareDir,
		ReferenceName: plumbing.ReferenceName("refs/heads/main"),
	}); err != nil {
		t.Fatalf("clone seeded bare: %v", err)
	}
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
	if _, err := gogit.PlainOpen(bareDir); err != nil {
		t.Fatalf("InitBareEmpty did not produce an openable bare: %v", err)
	}
	if _, err := os.Stat(filepath.Join(bareDir, "HEAD")); err != nil {
		t.Errorf("bare HEAD missing: %v", err)
	}
}

func TestPromoteWorkingTreeToBare(t *testing.T) {
	wt := filepath.Join(t.TempDir(), "wt")
	if err := os.MkdirAll(wt, 0755); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInitWithOptions(wt, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatalf("init wt: %v", err)
	}
	if err := os.WriteFile(filepath.Join(wt, "f.txt"), []byte("x"), 0644); err != nil {
		t.Fatal(err)
	}
	w, _ := repo.Worktree()
	w.Add("f.txt")
	w.Commit("init", &gogit.CommitOptions{})

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
	cfg, _ := repo.Config()
	origin, ok := cfg.Remotes["origin"]
	if !ok {
		t.Fatal("origin remote not set")
	}
	if origin.URLs[0] != bare {
		t.Errorf("origin URL: got %q, want %q", origin.URLs[0], bare)
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
