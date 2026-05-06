package project

// Tests for PromoteWorkingTreeToBare — the primitive that
// `enju bot setup` calls to give an adopted-folder project a
// bare push target. Bots can then push branches into the bare
// without hitting git's "branch is currently checked out" rule.

import (
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
)

// makeWorkingTreeWithBranches sets up a git repo at dir with a
// commit on `main`, plus extra branches if names is non-empty.
// Used to simulate the operator's adopted folder.
func makeWorkingTreeWithBranches(t *testing.T, dir string, extraBranches ...string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# op\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Commit("seed", &gogit.CommitOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	headRef, _ := repo.Head()
	for _, name := range extraBranches {
		ref := plumbing.NewHashReference(plumbing.NewBranchReferenceName(name), headRef.Hash())
		if err := repo.Storer.SetReference(ref); err != nil {
			t.Fatal(err)
		}
	}
}

func TestPromoteWorkingTreeToBare_HappyPath(t *testing.T) {
	tmp := t.TempDir()
	opTree := filepath.Join(tmp, "op-tree")
	bare := filepath.Join(tmp, "repos", "demo-3.git")
	makeWorkingTreeWithBranches(t, opTree, "smoke-1", "smoke-2")

	if err := PromoteWorkingTreeToBare(opTree, bare); err != nil {
		t.Fatalf("PromoteWorkingTreeToBare: %v", err)
	}

	// 1. Bare exists and is openable.
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	cfg, _ := bareRepo.Config()
	if !cfg.Core.IsBare {
		t.Errorf("expected bare repo, got non-bare")
	}

	// 2. All branches from the operator's tree made it into the bare.
	wantBranches := map[string]bool{"main": false, "smoke-1": false, "smoke-2": false}
	iter, err := bareRepo.Branches()
	if err != nil {
		t.Fatal(err)
	}
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		short := ref.Name().Short()
		if _, ok := wantBranches[short]; ok {
			wantBranches[short] = true
		}
		return nil
	})
	for name, found := range wantBranches {
		if !found {
			t.Errorf("bare missing branch %q", name)
		}
	}

	// 3. Operator's tree has origin pointing at the bare.
	opRepo, _ := gogit.PlainOpen(opTree)
	opCfg, _ := opRepo.Config()
	origin, ok := opCfg.Remotes["origin"]
	if !ok {
		t.Fatal("operator tree has no origin after promote")
	}
	if len(origin.URLs) == 0 || origin.URLs[0] != bare {
		t.Errorf("origin URL: got %v, want [%s]", origin.URLs, bare)
	}
}

func TestPromoteWorkingTreeToBare_Idempotent(t *testing.T) {
	// Running it twice must be a no-op (no error, no re-clone).
	tmp := t.TempDir()
	opTree := filepath.Join(tmp, "op-tree")
	bare := filepath.Join(tmp, "repos", "demo.git")
	makeWorkingTreeWithBranches(t, opTree)

	if err := PromoteWorkingTreeToBare(opTree, bare); err != nil {
		t.Fatal(err)
	}
	// Capture the bare's mtime to detect re-clone (a re-clone
	// would touch the dir again).
	bareInfo1, _ := os.Stat(filepath.Join(bare, "HEAD"))
	if err := PromoteWorkingTreeToBare(opTree, bare); err != nil {
		t.Fatalf("second call should be a no-op, got: %v", err)
	}
	bareInfo2, _ := os.Stat(filepath.Join(bare, "HEAD"))
	if !bareInfo1.ModTime().Equal(bareInfo2.ModTime()) {
		t.Errorf("second call re-cloned the bare (HEAD mtime changed)")
	}
}

func TestPromoteWorkingTreeToBare_ReplacesExistingOrigin(t *testing.T) {
	// If operator's tree had `origin` pointing somewhere else
	// (e.g. an old github URL they're abandoning), the promote
	// should replace it. Defensive: avoids a stale origin
	// silently routing future pushes to the wrong place.
	tmp := t.TempDir()
	opTree := filepath.Join(tmp, "op-tree")
	bare := filepath.Join(tmp, "repos", "demo.git")
	makeWorkingTreeWithBranches(t, opTree)

	opRepo, _ := gogit.PlainOpen(opTree)
	if _, err := opRepo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{"https://example.com/old-remote.git"}}); err != nil {
		t.Fatal(err)
	}

	if err := PromoteWorkingTreeToBare(opTree, bare); err != nil {
		t.Fatal(err)
	}

	opRepo, _ = gogit.PlainOpen(opTree)
	cfg, _ := opRepo.Config()
	if cfg.Remotes["origin"].URLs[0] != bare {
		t.Errorf("expected origin replaced with %q, got %v", bare, cfg.Remotes["origin"].URLs)
	}
}

func TestPromoteWorkingTreeToBare_RejectsNonGitDir(t *testing.T) {
	tmp := t.TempDir()
	notARepo := filepath.Join(tmp, "plain-dir")
	_ = os.MkdirAll(notARepo, 0o755)
	bare := filepath.Join(tmp, "repos", "demo.git")

	err := PromoteWorkingTreeToBare(notARepo, bare)
	if err == nil {
		t.Fatal("expected error when source is not a git working tree")
	}
}
