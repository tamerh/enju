package enjugit

import (
	"errors"
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

// nullLogger discards everything.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initBareForWorkspaceTest creates a bare git repo with one
// initial commit on main. Returns the bare path.
func initBareForWorkspaceTest(t *testing.T) string {
	t.Helper()
	bare := t.TempDir()
	_, err := gogit.PlainInitWithOptions(bare, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
		Bare:        true,
	})
	if err != nil {
		t.Fatalf("init bare: %v", err)
	}
	// Seed.
	seed := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(seed, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bare},
	}); err != nil {
		t.Fatal(err)
	}
	wt, _ := repo.Worktree()
	os.WriteFile(filepath.Join(seed, "README.md"), []byte("# seed\n"), 0o644)
	wt.Add("README.md")
	sig := &object.Signature{Name: "T", Email: "t@x", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("seed", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatal(err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatal(err)
	}
	return bare
}

func TestNewWorkspace_DefaultsToHome(t *testing.T) {
	// Use an explicit dir so we don't pollute ~ during tests.
	dir := t.TempDir()
	ws, err := NewWorkspace(dir, NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatalf("NewWorkspace: %v", err)
	}
	if ws.RootDir() != dir {
		t.Errorf("RootDir: got %s, want %s", ws.RootDir(), dir)
	}
}

func TestForProject_FreshClone(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, err := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	if err != nil {
		t.Fatal(err)
	}
	wf, err := ws.ForProject(7, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}
	if wf.ProjectID() != 7 {
		t.Errorf("ProjectID: got %d, want 7", wf.ProjectID())
	}
	// Cached: second call returns same handle.
	wf2, _ := ws.ForProject(7, bare)
	if wf != wf2 {
		t.Error("ForProject should cache by id")
	}
	if !ws.HasLocalClone(7) {
		t.Error("HasLocalClone should be true after ForProject")
	}
}

func TestForProject_NoSource_InitsLocal(t *testing.T) {
	// Empty remoteURL is the "solo / no-remote" project mode.
	// ForProject inits a local-only clone (with seed) so the
	// workflow is usable without an upstream. Callers can wire
	// origin later via SetRemote.
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(7, "")
	if err != nil {
		t.Fatalf("ForProject with empty remoteURL: expected local-init, got %v", err)
	}
	if wf == nil {
		t.Fatal("ForProject returned nil workflow")
	}
	if !ws.HasLocalClone(7) {
		t.Error("HasLocalClone should be true after local-init")
	}
}

func TestOpenView_CloneNotFound(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	_, err := ws.OpenView(7)
	if !errors.Is(err, ErrCloneNotFound) {
		t.Errorf("expected ErrCloneNotFound, got %v", err)
	}
}

func TestOpenOrLazyClone_LazyClonesWhenMissing(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	v, err := ws.OpenOrLazyClone(7, bare)
	if err != nil {
		t.Fatalf("OpenOrLazyClone: %v", err)
	}
	if v.ProjectID() != 7 {
		t.Errorf("ProjectID: got %d, want 7", v.ProjectID())
	}
}

func TestOpenOrLazyClone_NoSource(t *testing.T) {
	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	_, err := ws.OpenOrLazyClone(7, "")
	if !errors.Is(err, ErrNoCloneSource) {
		t.Errorf("expected ErrNoCloneSource, got %v", err)
	}
}

func TestProductionBranchName(t *testing.T) {
	convs := NewProductionConventions()
	got := convs.BranchName(2, "build", "develop_a", "", 3)
	want := "2-build/develop_a/iter-3"
	if got != want {
		t.Errorf("BranchName: got %q, want %q", got, want)
	}
	// With instance key.
	got = convs.BranchName(1, "build", "review", "module-x", 1)
	want = "1-build/module-x/review/iter-1"
	if got != want {
		t.Errorf("BranchName with instance: got %q, want %q", got, want)
	}
}

func TestProductionDiskLayout(t *testing.T) {
	convs := NewProductionConventions()
	if got := convs.DiskLayout.BarePath("/proj"); got != "/proj/enju/.bare.git" {
		t.Errorf("BarePath: got %q", got)
	}
	if got := convs.DiskLayout.BotClonePath("/proj", "alice"); got != "/proj/enju/bots/alice/clone" {
		t.Errorf("BotClonePath: got %q", got)
	}
	if got := convs.DiskLayout.OperatorClonePath("/proj"); got != "/proj/enju/.clone" {
		t.Errorf("OperatorClonePath: got %q", got)
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"hello":          "hello",
		"Hello World":    "hello-world",
		"foo!bar@baz":    "foo-bar-baz",
		"   trim   me  ": "trim-me",
		"":               "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q): got %q, want %q", in, want, got)
		}
	}
}
