package workspace

// Tests for ForceManagedClone — the bot daemon's
// guaranteed-managed-clone resolution path. The non-obvious
// behavior pinned here: ForceManagedClone EVICTS a cached
// Project whose WorkDir doesn't match the expected managed-
// clone path. Without that strict cache check, an earlier
// ForProject call (which honors externalDirs) poisons the
// cache and ForceManagedClone silently returns the operator's
// adopted dir.
//
// See ISSUE-007 / ISSUE-010 follow-up: bot daemon kept ending
// up in the operator's tree because ClaimTask's pre-claim
// reconcile populated ws.clients[id] with the externalDirs
// path before the daemon's own ResolveBotWorkspace ran.

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

// initBareSourceRepo plants a usable upstream the managed
// clone can pull from. Returns the path.
func initBareSourceRepo(t *testing.T, dir string) string {
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
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("# upstream\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, _ = wt.Add("README.md")
	if _, err := wt.Commit("seed", &gogit.CommitOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestForceManagedClone_EvictsCachedNonManagedProject(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	wsRoot := filepath.Join(tmp, "ws")
	if err := os.MkdirAll(wsRoot, 0o755); err != nil {
		t.Fatal(err)
	}
	ws, err := NewWorkspace(wsRoot, logger)
	if err != nil {
		t.Fatal(err)
	}

	// Operator's adopted dir lives outside the workspace root.
	adoptedDir := initBareSourceRepo(t, filepath.Join(tmp, "operator-tree"))

	// Step 1: simulate the production bug's ordering — a prior
	// ForProject call (e.g. ClaimTask's pre-claim reconcile)
	// resolves to the operator's tree via the externalDirs
	// short-circuit and caches it.
	ws.RegisterExternalDir(42, adoptedDir)
	pBad, err := ws.ForProject(42, adoptedDir, "demo")
	if err != nil {
		t.Fatalf("ForProject (priming the bad cache): %v", err)
	}
	if pBad.WorkDir() != adoptedDir {
		t.Fatalf("priming step did not cache adopted dir; got %q", pBad.WorkDir())
	}

	// Step 2: now the daemon's ResolveBotWorkspace fires.
	// Pre-fix this returned the cached operator-tree handle.
	// Post-fix it must EVICT and re-clone to the managed path.
	pGood, err := ws.ForceManagedClone(42, adoptedDir, "demo")
	if err != nil {
		t.Fatalf("ForceManagedClone: %v", err)
	}
	if pGood.WorkDir() == adoptedDir {
		t.Fatalf("ForceManagedClone returned cached operator-tree path %q (eviction failed)", pGood.WorkDir())
	}
	wantPrefix := wsRoot
	if !startsWith(pGood.WorkDir(), wantPrefix) {
		t.Errorf("ForceManagedClone WorkDir %q should live under workspace root %q", pGood.WorkDir(), wantPrefix)
	}
	if _, err := os.Stat(filepath.Join(pGood.WorkDir(), ".git")); err != nil {
		t.Errorf("managed clone has no .git: %v", err)
	}

	// Step 3: externalDirs entry for that ID should also be
	// gone — otherwise a SECOND ForProject call right after
	// would re-bind to the operator's tree and re-poison the
	// cache the next time the daemon's claim flow runs.
	ws.mu.Lock()
	_, stillRegistered := ws.externalDirs[42]
	ws.mu.Unlock()
	if stillRegistered {
		t.Errorf("ForceManagedClone should drop externalDirs[42] on eviction")
	}
}

func TestForceManagedClone_ReturnsCachedManagedProject(t *testing.T) {
	// Round-trip: when ForceManagedClone is called twice in a
	// row and the cached entry IS the managed-clone path,
	// return it without re-cloning.
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	wsRoot := filepath.Join(tmp, "ws")
	_ = os.MkdirAll(wsRoot, 0o755)
	ws, _ := NewWorkspace(wsRoot, logger)

	upstream := initBareSourceRepo(t, filepath.Join(tmp, "upstream"))

	first, err := ws.ForceManagedClone(7, upstream, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	second, err := ws.ForceManagedClone(7, upstream, "alpha")
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Errorf("ForceManagedClone should return the SAME *Project on second call (cache hit), got distinct handles")
	}
}

func TestForceManagedClone_EmptyRemoteErrors(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tmp := t.TempDir()
	ws, _ := NewWorkspace(tmp, logger)

	_, err := ws.ForceManagedClone(7, "", "alpha")
	if err == nil {
		t.Fatal("expected error from empty remoteURL")
	}
	if !errors.Is(err, err) || err.Error() == "" {
		t.Errorf("expected non-empty error message")
	}
}

func startsWith(s, prefix string) bool {
	return len(s) >= len(prefix) && s[:len(prefix)] == prefix
}
