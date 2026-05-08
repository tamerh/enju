package project

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/enjugit"
	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"
)

// plumbingHash is a tiny wrapper so tests don't have to import
// go-git's plumbing package just to parse a hex SHA.
func plumbingHash(s string) plumbing.Hash { return plumbing.NewHash(s) }

// ResultDir rebuilds the canonical task-result path for
// workspace's own tests. The layout schema lives in engine
// (engine.ComputeResultDir), but workspace tests can't import
// engine without a cycle, so this test-local duplicate
// matches the schema by construction. If the engine schema
// changes, this helper must move in lockstep — protected by
// the layout tests in both packages asserting the same
// canonical paths.
func ResultDir(runSeq int, instanceKey, taskDefID string) string {
	base := filepath.Join("enju", "runs",
		// %d formatter avoided so test-local doesn't need fmt.
		intToString(runSeq), taskDefID)
	if instanceKey != "" {
		// Legacy instance-key form: tests use a single blob
		// (e.g. "BRCA1") rather than the key=value layout the
		// server emits. For the purposes of workspace's client
		// round-trip tests — which just need a unique
		// result-dir-shaped string — appending the key under
		// the task-def dir is enough. Test assertions use the
		// same helper on both sides so the format is
		// self-consistent within this package.
		return filepath.Join(base, instanceKey)
	}
	return base
}

func intToString(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var digits []byte
	for n > 0 {
		digits = append([]byte{byte('0' + n%10)}, digits...)
		n /= 10
	}
	if neg {
		digits = append([]byte{'-'}, digits...)
	}
	return string(digits)
}

// testSignature returns a deterministic signature for commits made
// inside tests. Using a fixed time avoids spurious non-determinism
// if anyone ever hashes commit metadata in assertions.
func testSig() *object.Signature {
	return &object.Signature{
		Name:  "Test",
		Email: "test@localhost",
		When:  time.Unix(1700000000, 0),
	}
}

// nullLogger returns a slog.Logger that discards everything. Used in
// tests to keep output clean.
func nullLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// initBareRemote creates a bare git repo that tests use as a fake
// "remote" origin. The bare is initialized with `refs/heads/main` as
// the default branch so subsequent clones find a HEAD to track after
// the first push. Returns the filesystem path (which go-git accepts
// as a URL for `file://`-style cloning).
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

// seedRemoteWithInitialCommit makes the bare repo look like a
// freshly-created project: one README.md commit on refs/heads/main.
// The bare's HEAD is already set to refs/heads/main by
// initBareRemote, so after this push clones can find it.
func seedRemoteWithInitialCommit(t *testing.T, bareDir string) {
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
	readme := filepath.Join(seedDir, "README.md")
	if err := os.WriteFile(readme, []byte("# seed\n"), 0644); err != nil {
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



// TestSubmitRetryOnConcurrentPush simulates a second client pushing
// between our fetch and our push. First SubmitTaskResult attempt
// encounters a stale base, the retry loop re-fetches, re-overlays,
// re-commits, and eventually pushes on attempt 2.
func TestSubmitRetryOnConcurrentPush(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Client A will submit first.
	wsA, _ := NewOpener(t.TempDir(), nullLogger())
	projA, err := wsA.ForProject(44, bare)
	if err != nil {
		t.Fatalf("clone A: %v", err)
	}

	// Client B clones and is ready to submit.
	wsB, _ := NewOpener(t.TempDir(), nullLogger())
	projB, err := wsB.ForProject(44, bare)
	if err != nil {
		t.Fatalf("clone B: %v", err)
	}

	// A submits first.
	projA.Lock()
	if _, err := projA.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:a",
		Username: "alice",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(1, "", "a"), "result.md"), Content: []byte("alice result")},
		},
	}); err != nil {
		t.Fatalf("A submit: %v", err)
	}
	projA.Unlock()

	// B now submits a different task. B's local clone doesn't know
	// about A's push yet — the retry loop should fetch + reset +
	// re-apply + push on attempt 2 (or even attempt 1, since our
	// resetToRemote runs at the start of each attempt).
	projB.Lock()
	res, err := projB.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:b",
		Username: "bob",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(1, "", "b"), "result.md"), Content: []byte("bob result")},
		},
	})
	projB.Unlock()
	if err != nil {
		t.Fatalf("B submit: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("expected non-empty commit SHA for B")
	}

	// Verify both A's and B's results are on the remote — the retry
	// loop must have preserved A's commit when pushing B's.
	verifyDir := t.TempDir()
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(1, "", "a"), "result.md")); err != nil {
		t.Fatalf("A's file missing after B submit: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(1, "", "b"), "result.md")); err != nil {
		t.Fatalf("B's file missing after B submit: %v", err)
	}
}









// TestPushForceOverwritesDivergedRemote covers the force-push
// recovery path used by the explicit force-sync MCP tool. We simulate
// a diverged remote by pointing two independently-seeded clients at
// the same bare repo, then verify that PushForce from the second
// client overwrites the first client's commit.
func TestPushForceOverwritesDivergedRemote(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	// Client A writes and pushes normally to bare.
	wsA, _ := NewOpener(t.TempDir(), nullLogger())
	projA, err := wsA.ForProject(60, bare)
	if err != nil {
		t.Fatalf("clone A: %v", err)
	}
	projA.Lock()
	if _, err := projA.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:a",
		Username: "alice",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(1, "", "a"), "result.md"), Content: []byte("alice v1")},
		},
	}); err != nil {
		t.Fatalf("A submit: %v", err)
	}
	projA.Unlock()

	// Client B starts on an unrelated bare (same seed, different
	// history). Write + commit locally so HEAD is a real commit.
	bareB := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bareB)
	wsB, _ := NewOpener(t.TempDir(), nullLogger())
	projB, err := wsB.ForProject(60, bareB)
	if err != nil {
		t.Fatalf("clone B: %v", err)
	}
	projB.Lock()
	if _, err := projB.SubmitTaskResult(SubmitRequest{
		TaskID:   "1:1:b",
		Username: "bob",
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(1, "", "b"), "result.md"), Content: []byte("bob v1")},
		},
	}); err != nil {
		t.Fatalf("B initial submit: %v", err)
	}
	projB.Unlock()

	// Repoint B at A's bare. Normal Push should fail (divergent
	// histories), PushForce should win.
	if err := projB.repo.DeleteRemote("origin"); err != nil {
		t.Fatalf("delete origin: %v", err)
	}
	if _, err := projB.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bare},
	}); err != nil {
		t.Fatalf("recreate origin: %v", err)
	}
	projB.remoteURL = bare

	projB.Lock()
	if err := projB.Push(); err == nil {
		t.Fatal("expected normal Push to fail against diverged remote")
	}
	if err := projB.PushForce(); err != nil {
		t.Fatalf("PushForce: %v", err)
	}
	projB.Unlock()

	verifyDir := t.TempDir()
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(1, "", "a"), "result.md")); !os.IsNotExist(err) {
		t.Errorf("expected A's file to be gone after force push, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, ResultDir(1, "", "b"), "result.md")); err != nil {
		t.Errorf("expected B's file on remote after force push: %v", err)
	}
}

// TestSubmitFailsClearlyAgainstUnreachableRemote verifies that
// a non-recoverable push failure (bogus remote) surfaces a
// clean error naming the actual failure (push) and carrying
// the underlying reason, without retrying uselessly. The old
// design looped through a "sync → commit → push" retry and
// labeled the error by step; the new design commits on top of
// local HEAD once (preserving any user commits) and only
// retries when the failure is a non-fast-forward rejection
// that a rebase could fix. A missing-repository error is NOT
// non-FF, so we return immediately with a clear message.
func TestSubmitFailsClearlyAgainstUnreachableRemote(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, _ := NewOpener(t.TempDir(), nullLogger())
	proj, err := ws.ForProject(61, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Point the project at a bogus remote so push fails with
	// "repository not found" — a hard error the retry loop
	// cannot recover from.
	if err := proj.repo.DeleteRemote("origin"); err != nil {
		t.Fatalf("delete origin: %v", err)
	}
	bogus := filepath.Join(t.TempDir(), "nonexistent.git")
	if _, err := proj.repo.CreateRemote(&config.RemoteConfig{
		Name: "origin",
		URLs: []string{bogus},
	}); err != nil {
		t.Fatalf("recreate origin: %v", err)
	}
	proj.remoteURL = bogus

	proj.Lock()
	_, err = proj.SubmitTaskResult(SubmitRequest{
		TaskID:     "1:1:x",
		Username:   "alice",
		MaxRetries: 2,
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(ResultDir(1, "", "x"), "result.md"), Content: []byte("data")},
		},
	})
	proj.Unlock()
	if err == nil {
		t.Fatal("expected submit to fail against bogus remote")
	}
	msg := err.Error()
	// The error must surface the push step and the underlying
	// repository-not-found reason so users can diagnose.
	if !strings.Contains(msg, "push failed") {
		t.Errorf("expected 'push failed' prefix in error, got: %q", msg)
	}
	if !strings.Contains(msg, "not found") {
		t.Errorf("expected underlying 'not found' reason in error, got: %q", msg)
	}
}

// TestCrossWorkspaceFlockSerialization verifies that two Workspace
// instances pointed at the same root dir (simulating two MCP
// processes running against the same ~/.enju/workspaces) serialize
// their Project.Lock() calls via the on-disk flock. The second
// Lock must block until the first Unlock happens.
func TestCrossWorkspaceFlockSerialization(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	sharedRoot := t.TempDir()

	wsA, _ := NewOpener(sharedRoot, nullLogger())
	projA, err := wsA.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsA ForProject: %v", err)
	}

	wsB, _ := NewOpener(sharedRoot, nullLogger())
	projB, err := wsB.ForProject(80, bare)
	if err != nil {
		t.Fatalf("wsB ForProject: %v", err)
	}

	// Sanity: different in-process handles (each workspace has its
	// own clients map), but pointing at the same clone on disk.
	if projA == projB {
		t.Fatal("expected distinct Project instances across Workspaces")
	}
	if projA.WorkDir() != projB.WorkDir() {
		t.Fatalf("expected same work dir across Workspaces, got %q vs %q",
			projA.WorkDir(), projB.WorkDir())
	}

	// A locks first.
	projA.Lock()

	// B tries to lock — should block until A unlocks. Run it in a
	// goroutine and observe it's still waiting after a short
	// moment.
	done := make(chan struct{})
	go func() {
		projB.Lock()
		close(done)
	}()

	select {
	case <-done:
		t.Fatal("projB.Lock() returned while projA was still holding the lock")
	case <-time.After(50 * time.Millisecond):
		// Expected: B is blocked on A.
	}

	projA.Unlock()

	select {
	case <-done:
		// Good: B acquired once A released.
	case <-time.After(2 * time.Second):
		t.Fatal("projB.Lock() never returned after projA.Unlock()")
	}
	projB.Unlock()
}

// TestSlugifyProjectDir verifies that ForProject with a project name
// creates a "{slug}-{id}" directory, and that an existing numeric-only
// directory is auto-migrated to the slug form.
func TestSlugifyProjectDir(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	wsDir := t.TempDir()
	ws, _ := NewOpener(wsDir, nullLogger())

	// Case 1: passing a project name creates a slug-based dir.
	proj, err := ws.ForProject(7, bare, "Battle Test Alpha")
	if err != nil {
		t.Fatalf("clone with name: %v", err)
	}
	expected := filepath.Join(wsDir, "battle-test-alpha-7")
	if proj.WorkDir() != expected {
		t.Errorf("expected workdir %s, got %s", expected, proj.WorkDir())
	}
	if ws.findProjectDir(7) == "" {
		t.Error("findProjectDir should find slug-named dir")
	}

	// Case 2: legacy numeric dir gets auto-migrated.
	// Create a numeric-only clone, then call ForProject with a name.
	ws2, _ := NewOpener(t.TempDir(), nullLogger())
	projOld, err := ws2.ForProject(8, bare) // no name → numeric dir
	if err != nil {
		t.Fatalf("clone without name: %v", err)
	}
	numericDir := projOld.WorkDir()
	if filepath.Base(numericDir) != "8" {
		t.Fatalf("expected numeric dir '8', got %s", filepath.Base(numericDir))
	}
	// Clear cached handle so ForProject re-resolves the directory.
	ws2.mu.Lock()
	delete(ws2.clients, 8)
	ws2.mu.Unlock()
	// Now call with a name — should rename the directory.
	proj2, err := ws2.ForProject(8, bare, "My Project")
	if err != nil {
		t.Fatalf("reopen with name: %v", err)
	}
	if filepath.Base(proj2.WorkDir()) != "my-project-8" {
		t.Errorf("expected migrated dir 'my-project-8', got %s", filepath.Base(proj2.WorkDir()))
	}
	// Old numeric dir should be gone.
	if _, err := os.Stat(numericDir); !os.IsNotExist(err) {
		t.Error("expected old numeric dir to be gone after migration")
	}
}

// errStr is a tiny test helper: build an error from a literal string
// without pulling in errors.New at every call site.
func errStr(s string) error { return &stringErr{s} }

type stringErr struct{ msg string }

func (e *stringErr) Error() string { return e.msg }

// commitToDefault is a test helper that lands a set of files on
// the project's default branch in one commit + push. Template
// discovery reads through the default-branch tree, so tests that
// exercise ListTemplates/LoadTemplate/LoadProjectConfig have to
// commit their fixtures rather than leaving them as worktree
// files.
func commitToDefault(t *testing.T, proj *Clone, files map[string][]byte) {
	t.Helper()
	writes := make([]FileWrite, 0, len(files))
	for path, body := range files {
		writes = append(writes, FileWrite{
			RepoRelPath: path,
			Content:     body,
			Mode:        0o644,
		})
	}
	proj.Lock()
	defer proj.Unlock()
	if _, err := proj.CommitFiles(CommitFilesRequest{
		Files:       writes,
		CommitMsg:   "seed test fixtures",
		AuthorName:  "t",
		AuthorEmail: "t@x",
	}); err != nil {
		t.Fatalf("commitToDefault: %v", err)
	}
}
