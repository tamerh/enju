package mcpserver

// Concurrency-invariant tests for the reconcile hook. The
// main correctness claim these protect: proj.Lock is released
// BEFORE the HTTP POST to /tasks/reconcile, so concurrent
// goroutines touching the same project don't block on the
// network round-trip. A regression here would be invisible
// under serial load but show up as surprising latency cliffs
// once a scanner's reconcile batch and another tool call
// overlap.
//
// Uses httptest to inject a slow /tasks/reconcile handler and
// observes that a separate proj.Lock acquisition succeeds
// while the slow POST is in flight.

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/config"
	"github.com/go-git/go-git/v5/plumbing"
	"github.com/go-git/go-git/v5/plumbing/object"

	"github.com/enju-ai/enju/internal/mcpgit"
)

func testCtx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func writeFile(dir, name, content string) error {
	return os.WriteFile(filepath.Join(dir, name), []byte(content), 0644)
}

// TestPullBranchWithReconcileReleasesLockAcrossPost is the
// direct regression guard for the tester's concern #3: if a
// future refactor accidentally pulls the HTTP POST back inside
// proj.Lock, this test will stall (lock held for the entire
// slow-handler duration instead of released after the git
// phase).
//
// Construction:
//   - Slow handler on /api/v1/tasks/reconcile that blocks
//     until we signal it.
//   - A bare remote with one trailer-tagged commit (so the
//     scanner has something to post).
//   - Kick pullBranchWithReconcile in goroutine A.
//   - From the main goroutine, wait until the POST is in
//     flight, then try to acquire proj.Lock. If the lock is
//     released (correct), acquisition is instant; if held
//     across POST (bug), we'd wait for the slow handler to
//     finish.
//   - Cancel the handler, verify goroutine A completes cleanly.
func TestPullBranchWithReconcileReleasesLockAcrossPost(t *testing.T) {
	// --- Slow reconcile handler ---
	postEntered := make(chan struct{})
	unblockPost := make(chan struct{})
	var handlerOnce sync.Once
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/tasks/reconcile" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		handlerOnce.Do(func() { close(postEntered) })
		<-unblockPost
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]interface{}{"results": []interface{}{}})
	}))
	defer srv.Close()

	// --- Bare remote. Seed with an initial non-trailer
	//     commit, then a fat-client clone, then a NEW
	//     trailer commit on origin. First-scan baseline
	//     would otherwise skip the whole walk — by
	//     pre-seeding the cursor (implicitly via the initial
	//     fetch + a no-op scan call) we force the next scan
	//     to actually walk commits and emit trailers. ---
	bare := t.TempDir()
	if _, err := gogit.PlainInitWithOptions(bare, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
		Bare:        true,
	}); err != nil {
		t.Fatalf("init bare: %v", err)
	}
	seedInitialCommit(t, bare) // non-trailer baseline commit

	// --- Fat-client workspace rooted in a temp dir. ---
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	wsRoot := t.TempDir()
	ws, err := mcpgit.NewWorkspace(wsRoot, logger)
	if err != nil {
		t.Fatalf("workspace: %v", err)
	}
	const projectID int64 = 77
	proj, err := ws.ForProject(projectID, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Pre-seed the cursor to the current origin/main tip so
	// the next scan walks commits beyond that, not the
	// first-scan baseline branch. Without this the scanner
	// returns (tip, nil, nil) and never posts.
	if err := proj.FetchBranch("main"); err != nil {
		t.Fatalf("seed fetch: %v", err)
	}
	baselineTip, _, err := proj.ScanBranchSince("main", "")
	if err != nil {
		t.Fatalf("baseline scan: %v", err)
	}
	stateDir := filepath.Join(wsRoot, ".state")
	cursors := mcpgit.NewCursors(stateDir, projectID)
	cursors.Set("main", baselineTip)
	if err := cursors.Save(); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	// NOW land a trailer-tagged commit on the bare. The
	// next scan will walk it and trigger the POST.
	seedTrailerCommit(t, bare)

	// apiClient pointed at the slow server. No citizen /
	// token wiring — the slow handler ignores auth.
	c := &apiClient{
		baseURL:    srv.URL,
		username:   "t",
		logger:     logger,
		workspace:  ws,
		httpClient: http.DefaultClient,
	}

	// --- Goroutine A: pullBranchWithReconcile drives the
	//     POST through the slow handler. ---
	done := make(chan error, 1)
	go func() {
		done <- c.pullBranchWithReconcile(testCtx(t), proj, 77, "main")
	}()

	// --- Wait for the POST handler to be entered. ---
	select {
	case <-postEntered:
	case <-time.After(5 * time.Second):
		close(unblockPost)
		<-done
		t.Fatalf("reconcile POST never reached the slow handler")
	}

	// --- The invariant under test: proj.Lock is NOT held
	//     while the POST blocks. Acquire from the main
	//     goroutine with a short timeout. If the lock is
	//     held across the POST, this will wait for
	//     unblockPost (which we haven't closed yet). ---
	lockAcquired := make(chan struct{})
	go func() {
		proj.Lock()
		close(lockAcquired)
		proj.Unlock()
	}()
	select {
	case <-lockAcquired:
		// Good — lock was released before the POST.
	case <-time.After(500 * time.Millisecond):
		close(unblockPost) // unblock goroutine A so the test exits cleanly
		<-done
		t.Fatal("proj.Lock was held across the reconcile POST (expected to be released after the git phase)")
	}

	// --- Release the slow handler and verify goroutine A
	//     completes without error (no regression in the
	//     happy path). ---
	close(unblockPost)
	if err := <-done; err != nil {
		t.Errorf("pullBranchWithReconcile returned unexpected error: %v", err)
	}
}

// seedInitialCommit pushes a baseline (no-trailer) commit to
// the bare remote so first-fetch has something to pull.
// Used before seedTrailerCommit so the fat-client can
// establish a cursor pre-trailer, forcing the next scan to
// actually walk the trailer commit.
func seedInitialCommit(t *testing.T, bare string) {
	t.Helper()
	seedDir := t.TempDir()
	repo, err := gogit.PlainInitWithOptions(seedDir, &gogit.PlainInitOptions{
		InitOptions: gogit.InitOptions{DefaultBranch: plumbing.ReferenceName("refs/heads/main")},
	})
	if err != nil {
		t.Fatalf("init seed: %v", err)
	}
	if _, err := repo.CreateRemote(&config.RemoteConfig{Name: "origin", URLs: []string{bare}}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	wt, _ := repo.Worktree()
	_ = writeFile(seedDir, "README.md", "# seed\n")
	_, _ = wt.Add("README.md")
	sig := &object.Signature{Name: "t", Email: "t@localhost", When: time.Unix(1700000000, 0)}
	if _, err := wt.Commit("seed baseline", &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push seed: %v", err)
	}
}

// seedTrailerCommit clones the (already-seeded) bare remote,
// lands one new commit with an Enju-Task-Complete trailer,
// and pushes. Returns after the trailer commit is visible on
// origin — the fetch-path scanner will find it and post to
// /tasks/reconcile. Clones the bare (rather than init-from-
// empty) so the new commit is a descendant of the baseline
// and the push fast-forwards cleanly.
func seedTrailerCommit(t *testing.T, bare string) {
	t.Helper()
	workDir := t.TempDir()
	repo, err := gogit.PlainClone(workDir, false, &gogit.CloneOptions{URL: bare})
	if err != nil {
		t.Fatalf("clone seed: %v", err)
	}
	wt, _ := repo.Worktree()
	_ = writeFile(workDir, "note.txt", "hello")
	_, _ = wt.Add("note.txt")
	msg := "Task 99:1:ghost by @t: result\n\nEnju-Task-Complete: 99:1:ghost\nEnju-Exit: 0"
	sig := &object.Signature{Name: "t", Email: "t@localhost", When: time.Unix(1700000001, 0)}
	if _, err := wt.Commit(msg, &gogit.CommitOptions{Author: sig, Committer: sig}); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if err := repo.Push(&gogit.PushOptions{RemoteName: "origin"}); err != nil {
		t.Fatalf("push: %v", err)
	}
}
