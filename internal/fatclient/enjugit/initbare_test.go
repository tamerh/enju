package enjugit

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
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

// TestPromoteWorkingTreeToBare_TransfersAllBranches exercises the
// mirror-refspec path inside PromoteWorkingTreeToBare. The basic
// TestPromoteWorkingTreeToBare above only seeds main, so it
// wouldn't catch a regression where the bare ends up with HEAD-only
// (gogit's PlainClone(bare=true) default). Bots fork topic branches
// off main → if the bare is missing the operator's other branches,
// pushes from those branches break. Originally lived in project
// package as TestPromoteWorkingTreeToBare_HappyPath.
func TestPromoteWorkingTreeToBare_TransfersAllBranches(t *testing.T) {
	tmp := t.TempDir()
	wt := filepath.Join(tmp, "op-tree")
	bare := filepath.Join(tmp, "repos", "demo.git")

	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInitWithOptions(wt, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := repo.Worktree()
	if err := os.WriteFile(filepath.Join(wt, "README.md"), []byte("# op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	w.Add("README.md")
	w.Commit("seed", &gogit.CommitOptions{All: true})
	headRef, _ := repo.Head()
	for _, name := range []string{"smoke-1", "smoke-2"} {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), headRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatal(err)
		}
	}

	if err := PromoteWorkingTreeToBare(wt, bare); err != nil {
		t.Fatalf("PromoteWorkingTreeToBare: %v", err)
	}

	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	cfg, _ := bareRepo.Config()
	if !cfg.Core.IsBare {
		t.Errorf("expected bare repo, got non-bare")
	}
	wantBranches := map[string]bool{"main": false, "smoke-1": false, "smoke-2": false}
	iter, _ := bareRepo.Branches()
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		if _, ok := wantBranches[ref.Name().Short()]; ok {
			wantBranches[ref.Name().Short()] = true
		}
		return nil
	})
	for name, found := range wantBranches {
		if !found {
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
	if err := os.MkdirAll(wt, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInitWithOptions(wt, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	w, _ := repo.Worktree()
	os.WriteFile(filepath.Join(wt, "README.md"), []byte("# op\n"), 0o644)
	w.Add("README.md")
	w.Commit("seed", &gogit.CommitOptions{All: true})

	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{"https://example.com/old-remote.git"},
	}); err != nil {
		t.Fatal(err)
	}

	if err := PromoteWorkingTreeToBare(wt, bare); err != nil {
		t.Fatal(err)
	}

	repo, _ = gogit.PlainOpen(wt)
	cfg, _ := repo.Config()
	if got := cfg.Remotes["origin"].URLs[0]; got != bare {
		t.Errorf("expected origin replaced with %q, got %v", bare, got)
	}
}

// TestPromoteWorkingTreeToBare_RejectsNonGitDir verifies the input
// validation: pointing the promote at a directory that isn't a git
// working tree must return an error rather than silently corrupting
// the destination bare. Originally a separate test in project package.
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
