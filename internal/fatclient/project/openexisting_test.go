package project

// Tests for OpenExisting + the slug-form preference in
// findProjectDir. The bug these fix is documented in
// OpenExisting's doc: ForProject(id, "") could silently init
// an empty stub at "{rootDir}/{id}" when the real clone lived
// at the slug form, then findProjectDir would return the stub
// alphabetically.

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

func newWS(t *testing.T) *Opener {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ws, err := NewOpener(t.TempDir(), logger)
	if err != nil {
		t.Fatal(err)
	}
	return ws
}

// initRealRepo plants a slug-form clone with one commit so the
// test can verify OpenExisting picks IT and not the orphan.
func initRealRepo(t *testing.T, root, slug string, projectID int64) string {
	t.Helper()
	dir := filepath.Join(root, slug+"-"+itoa(projectID))
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
	readme := filepath.Join(dir, "README.md")
	_ = os.WriteFile(readme, []byte("# real\n"), 0o644)
	_, _ = wt.Add("README.md")
	if _, err := wt.Commit("seed", &gogit.CommitOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	digits := []byte{}
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	return string(digits)
}

func TestOpenExisting_NoCloneReturnsErrCloneNotFound(t *testing.T) {
	ws := newWS(t)
	_, err := ws.OpenExisting(7)
	if !errors.Is(err, ErrCloneNotFound) {
		t.Errorf("expected ErrCloneNotFound, got %v", err)
	}
}

func TestOpenExisting_FindsSlugClone(t *testing.T) {
	ws := newWS(t)
	want := initRealRepo(t, ws.RootDir(), "webui-toy", 1)

	proj, err := ws.OpenExisting(1)
	if err != nil {
		t.Fatalf("OpenExisting: %v", err)
	}
	if proj.workDir != want {
		t.Errorf("workDir mismatch: got %q, want %q", proj.workDir, want)
	}
}

func TestOpenExisting_DoesNotInit(t *testing.T) {
	// The bug: ForProject(id, "") could PlainInit a stub at
	// numeric-form when no clone existed. OpenExisting must
	// REFUSE to init — verify by asserting no directory
	// appears post-call.
	ws := newWS(t)
	root := ws.RootDir()
	if _, err := ws.OpenExisting(42); !errors.Is(err, ErrCloneNotFound) {
		t.Fatalf("expected ErrCloneNotFound, got %v", err)
	}
	entries, _ := os.ReadDir(root)
	for _, e := range entries {
		if e.Name() == "42" || e.Name() == "0" {
			t.Errorf("OpenExisting created a stub directory %q — should never init", e.Name())
		}
	}
}

func TestFindProjectDir_PrefersSlugOverNumeric(t *testing.T) {
	// The tie-break: with both slug-form and numeric-form
	// clones present (a real clone + an orphan stub from a
	// pre-fix buggy build), findProjectDir must pick the
	// slug. Alphabetical os.ReadDir would otherwise return
	// "1" before "webui-toy-1".
	ws := newWS(t)
	root := ws.RootDir()
	wantSlug := initRealRepo(t, root, "webui-toy", 1)
	// Plant a numeric-form orphan stub with its own .git dir
	// so findProjectDir's IsDir(.git) check accepts it.
	stub := filepath.Join(root, "1")
	if err := os.MkdirAll(filepath.Join(stub, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}

	got := ws.findProjectDir(1)
	if got != wantSlug {
		t.Errorf("findProjectDir tie-break failed: got %q, want %q (slug-form should beat numeric stub)", got, wantSlug)
	}
}

func TestFindProjectDir_NumericFallback(t *testing.T) {
	// Negative case for the tie-break: if ONLY the numeric
	// form exists, we still find it (legacy compat).
	ws := newWS(t)
	root := ws.RootDir()
	dir := filepath.Join(root, "1")
	if err := os.MkdirAll(filepath.Join(dir, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if got := ws.findProjectDir(1); got != dir {
		t.Errorf("numeric fallback broke: got %q, want %q", got, dir)
	}
}
