package git

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// nullLogger discards everything. Used in tests to keep output clean.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initBareRemote creates an empty bare git repo at a tempdir and
// returns its path. Default branch is main. Used as the "remote"
// for tests.
func initBareRemote(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	_, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
		Bare: true,
	})
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	return dir
}

// seedBareWithInitialCommit pushes one README.md commit on
// refs/heads/main into the bare so subsequent clones can branch
// from main.
func seedBareWithInitialCommit(t *testing.T, bareDir string) {
	t.Helper()
	seedDir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(seedDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bareDir},
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatalf("worktree: %v", err)
	}
	if err := os.WriteFile(filepath.Join(seedDir, "README.md"), []byte("# seed\n"), 0o644); err != nil {
		t.Fatalf("write readme: %v", err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatalf("add readme: %v", err)
	}
	sig := testSig()
	if _, err := wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push seed: %v", err)
	}
}

// testSig returns a deterministic git signature so tests don't
// depend on the runner's git config or wall clock.
func testSig() *object.Signature {
	return &object.Signature{
		Name:  "Test",
		Email: "test@localhost",
		When:  time.Unix(1700000000, 0),
	}
}

// freshClone clones a fresh working clone of bareDir into a
// tempdir and returns the open *Clone. lockPath is empty (no
// cross-process lock) so tests don't leak flock files.
func freshClone(t *testing.T, bareDir string) *Clone {
	t.Helper()
	workDir := t.TempDir()
	c, err := CloneOrInit(workDir, bareDir, "", nullLogger())
	if err != nil {
		t.Fatalf("CloneOrInit: %v", err)
	}
	return c
}

// commitOneFile writes path with content into c's worktree,
// commits it on the current branch, and returns the commit SHA.
// Used by tests to seed the local repo with content. Does not
// push.
func commitOneFile(t *testing.T, c *Clone, path string, content []byte) string {
	t.Helper()
	full := filepath.Join(c.workDir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(full, content, 0o644); err != nil {
		t.Fatal(err)
	}
	wt, err := c.repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add(path); err != nil {
		t.Fatal(err)
	}
	sig := testSig()
	h, err := wt.Commit("test commit", &gogit.CommitOptions{Author: sig, Committer: sig})
	if err != nil {
		t.Fatal(err)
	}
	return h.String()
}
