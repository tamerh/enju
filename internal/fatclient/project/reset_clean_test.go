package project

// Direct unit test for ResetWorktreeToCleanState. Bot daemons
// call this between iterations to clear residue from the
// previous task's claude -p run; the contract is "drop
// staged + unstaged tracked changes, remove untracked files,
// preserve .git infra."

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

// realCloneWithCommits initializes a local git repo with one
// committed README.md and returns the *Clone. Tests build
// scenarios on top — modifying files, adding untracked entries,
// staging deletions — then call ResetWorktreeToCleanState.
func realCloneWithCommits(t *testing.T) *Clone {
	t.Helper()
	dir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(dir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{
			DefaultBranch: plumbing.ReferenceName("refs/heads/main"),
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	wt, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# init\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.Add("README.md"); err != nil {
		t.Fatal(err)
	}
	sig := &object.Signature{Name: "tester", Email: "t@example.com"}
	if _, err := wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &Clone{repo: repo, workDir: dir, logger: logger}
}

// TestResetWorktreeToCleanState_RemovesUntrackedFiles —
// the simplest bot-residue scenario: previous task's claude
// -p left scratch files in the worktree (untracked). Reset
// removes them.
func TestResetWorktreeToCleanState_RemovesUntrackedFiles(t *testing.T) {
	p := realCloneWithCommits(t)
	if err := os.MkdirAll(filepath.Join(p.workDir, "src", "api"), 0o755); err != nil {
		t.Fatal(err)
	}
	residueFiles := []string{"src/api/server.go", "src/api/handler.go", "scratch.tmp"}
	for _, rel := range residueFiles {
		full := filepath.Join(p.workDir, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("residue"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	if err := p.ResetWorktreeToCleanState(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	for _, rel := range residueFiles {
		full := filepath.Join(p.workDir, filepath.FromSlash(rel))
		if _, err := os.Stat(full); err == nil {
			t.Errorf("residue file %s should be removed", rel)
		}
	}
	// Tracked file untouched.
	if _, err := os.Stat(filepath.Join(p.workDir, "README.md")); err != nil {
		t.Errorf("README.md (tracked) should remain: %v", err)
	}
}

// TestResetWorktreeToCleanState_DropsUnstagedModifications —
// claude -p modified a tracked file (e.g. go.mod) but the
// task's commit didn't capture the modification. Reset
// brings the worktree back to HEAD's content.
func TestResetWorktreeToCleanState_DropsUnstagedModifications(t *testing.T) {
	p := realCloneWithCommits(t)
	readme := filepath.Join(p.workDir, "README.md")
	original, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(readme, []byte("# modified by claude\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.ResetWorktreeToCleanState(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	got, err := os.ReadFile(readme)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Errorf("modification should be reverted: got %q, want %q", got, original)
	}
}

// TestResetWorktreeToCleanState_PreservesGitDir — `.git/`
// itself must survive. The infra-skip guard prevents the
// untracked-file walk from accidentally deleting refs or
// objects.
func TestResetWorktreeToCleanState_PreservesGitDir(t *testing.T) {
	p := realCloneWithCommits(t)
	// Drop a residue file alongside .git/ so the walk has a
	// reason to enter the directory containing it.
	if err := os.WriteFile(filepath.Join(p.workDir, "scratch"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := p.ResetWorktreeToCleanState(); err != nil {
		t.Fatalf("Reset: %v", err)
	}
	// .git/HEAD still readable.
	if _, err := os.Stat(filepath.Join(p.workDir, ".git", "HEAD")); err != nil {
		t.Errorf(".git/HEAD vanished: %v", err)
	}
	if _, err := os.Stat(filepath.Join(p.workDir, "scratch")); err == nil {
		t.Error("scratch should be removed")
	}
}

// TestResetWorktreeToCleanState_Idempotent — calling reset
// on an already-clean clone is a no-op. Daemon may call it
// every iteration; this asserts that doesn't churn anything.
func TestResetWorktreeToCleanState_Idempotent(t *testing.T) {
	p := realCloneWithCommits(t)
	for i := 0; i < 3; i++ {
		if err := p.ResetWorktreeToCleanState(); err != nil {
			t.Fatalf("call #%d: %v", i, err)
		}
	}
	// Tracked file still there, no extra files appeared.
	entries, err := os.ReadDir(p.workDir)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, e := range entries {
		names[e.Name()] = true
	}
	if !names["README.md"] || !names[".git"] {
		t.Errorf("missing core entries after idempotent resets: %v", names)
	}
	if len(names) != 2 {
		t.Errorf("unexpected entries: %v", names)
	}
}
