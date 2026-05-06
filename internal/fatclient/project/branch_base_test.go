package project

// Pin the fork-point picker for fresh branches. The contract is
// "use the project's default-branch HEAD," not "use the repo's
// root commit" — solo projects (no remote post-Option-B) trip
// the latter when the picker only inspects origin/* refs.

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// initSoloRepo plants a solo-project clone: one initial commit
// plus N follow-up commits on the local default branch, no
// remote configured. Returns the workDir and the hashes of the
// root commit and the latest main commit so the test can assert
// "branchBaseHash returned the latter, not the former."
func initSoloRepo(t *testing.T, defaultBranch string, extraCommits int) (workDir string, rootHash, headHash plumbing.Hash) {
	t.Helper()
	workDir = t.TempDir()
	repo, err := gogit.PlainInitWithOptions(workDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/" + defaultBranch),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "tester", Email: "t@example.com"}

	if err := os.WriteFile(filepath.Join(workDir, "README.md"), []byte("# init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	rootHash, err = wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}

	headHash = rootHash
	for i := 0; i < extraCommits; i++ {
		path := filepath.Join(workDir, "main-progress.txt")
		if err := os.WriteFile(path, []byte("update "+itoa(int64(i))), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, err := wt.Add("main-progress.txt"); err != nil {
			t.Fatal(err)
		}
		headHash, err = wt.Commit("update", &gogit.CommitOptions{Author: sig, Committer: sig})
		if err != nil {
			t.Fatal(err)
		}
	}
	return workDir, rootHash, headHash
}

// openSoloClone wraps an existing on-disk repo as a *Clone the
// way OpenExisting does for solo projects, sans workspace cache
// machinery. Just enough for branchBaseHash unit tests.
func openSoloClone(t *testing.T, workDir string) *Clone {
	t.Helper()
	repo, err := gogit.PlainOpen(workDir)
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Clone{
		repo:    repo,
		workDir: workDir,
		logger:  logger,
	}
}

// Pre-fix: solo projects (no origin/*) fell through to root.
// Post-fix: local refs/heads/<default> wins, so the latest main
// HEAD is returned and run branches fork from there.
func TestBranchBaseHash_SoloProjectUsesLocalDefaultHead(t *testing.T) {
	workDir, root, head := initSoloRepo(t, "main", 3)
	if root == head {
		t.Fatal("test setup: expected root != head with extra commits")
	}
	p := openSoloClone(t, workDir)
	got, err := p.branchBaseHash()
	if err != nil {
		t.Fatalf("branchBaseHash: %v", err)
	}
	if got != head {
		t.Errorf("fork point: got %s, want local main HEAD %s (root was %s — pre-fix bug)",
			got.String(), head.String(), root.String())
	}
}

// Pin that the picker honors the project's CONFIGURED default
// branch, not a hardcoded "main." A project initialized with
// `git init --initial-branch=trunk` and SetDefaultBranch("trunk")
// must fork from refs/heads/trunk, not fall through.
func TestBranchBaseHash_HonorsConfiguredDefaultBranch(t *testing.T) {
	workDir, _, head := initSoloRepo(t, "trunk", 2)
	p := openSoloClone(t, workDir)
	p.SetDefaultBranch("trunk")
	got, err := p.branchBaseHash()
	if err != nil {
		t.Fatalf("branchBaseHash: %v", err)
	}
	if got != head {
		t.Errorf("fork point: got %s, want trunk HEAD %s", got.String(), head.String())
	}
}
