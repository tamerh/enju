package enjugit

// Real-bare integration tests for SubmitTaskResult edge cases:
// concurrent-push retry, force-push over diverged remote, and
// clean failure against an unreachable remote. Each one drives
// SubmitTaskResult through the same go-git path production hits,
// covering scenarios the fake-ops unit tests in producing_test.go
// can't reach (real network/ref behavior).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
)

// TestSubmitTaskResult_ConcurrentPushSurfacesNonFFIntegration
// pins enjugit's CURRENT single-attempt behavior under a
// concurrent-push race: when client A pushes between B's clone
// and B's push, B's SubmitTaskResult returns a non-FF error
// instead of silently retrying.
//
// TODO(enjugit-retry-loop): the project package's old
// SubmitTaskResult had a retry loop here that fetched + reset +
// re-applied + re-pushed so both A's and B's commits landed on
// the bare. enjugit/producing.go intentionally dropped that loop
// ("single attempt; no rebase loop in v1" — see SubmitTaskResult
// body) and currently leaves retry to the caller. The original
// scenario was specifically protecting against the "naive force-
// push the loser" fix that drops one client's work — without the
// retry, callers must handle this themselves or risk lost commits.
// Revisit: either restore an opt-in retry loop on SubmitRequest
// (MaxRetries already exists in the struct but isn't honored), or
// make the higher-level service layer responsible for fetch+retry
// and document that contract clearly.
//
// For now this test asserts the current behavior — non-FF error
// surfaces with enough context for a caller to act on — so we
// don't lose the scenario entirely. When the retry loop comes
// back, the assertions flip: B's submit succeeds and both files
// land on the remote (the original assertion shape is preserved
// in this comment for reference).
func TestSubmitTaskResult_ConcurrentPushSurfacesNonFFIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	// Client A.
	wsA, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wfA, err := wsA.ForProject(44, bare)
	if err != nil {
		t.Fatalf("clone A: %v", err)
	}

	// Client B (separate workspace dir, same remote).
	wsB, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wfB, err := wsB.ForProject(44, bare)
	if err != nil {
		t.Fatalf("clone B: %v", err)
	}

	// A submits first.
	if _, err := wfA.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:a",
		BranchOverride: "main",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "a"), "result.md"), Content: []byte("alice result")},
		},
	}); err != nil {
		t.Fatalf("A submit: %v", err)
	}

	// B now submits a different task. B's local clone doesn't
	// know about A's push yet, so the push verifies as non-FF.
	// CURRENT behavior: error surfaces, no commit lands.
	// FUTURE behavior (when retry returns): B's submit succeeds.
	_, err = wfB.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:b",
		BranchOverride: "main",
		Citizen:        Identity{Name: "bob", Email: "bob@example.com"},
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "b"), "result.md"), Content: []byte("bob result")},
		},
	})
	if err == nil {
		t.Fatal("expected B's submit to fail with non-FF (single-attempt design); " +
			"if this assertion flips it means the retry loop returned — update the test to verify both files land on the bare")
	}
	msg := err.Error()
	if !strings.Contains(msg, "non-fast-forward") && !strings.Contains(msg, "non-FF") {
		t.Errorf("expected non-FF error wording, got: %q", msg)
	}

	// A's commit must still be on the remote — B's failed push
	// shouldn't have overwritten anything.
	verifyDir := t.TempDir()
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, resolveTestResultDir(1, "", "a"), "result.md")); err != nil {
		t.Fatalf("A's file missing after B's failed submit (B should not have overwritten): %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, resolveTestResultDir(1, "", "b"), "result.md")); !os.IsNotExist(err) {
		t.Errorf("B's file unexpectedly on remote after B's submit failed (single-attempt: should not have landed): err=%v", err)
	}
}

// TestPushForceOverwritesDivergedRemoteIntegration covers the
// force-push recovery path used by the explicit force-sync flow.
// Two clients build commits on independent bares, then we
// repoint client B at A's bare (creating divergent histories)
// and verify a force push wipes A's commit and lands B's tip.
//
// Uses git.Clone.PushAllRefs(force=true) since enjugit's
// public Workflow surface doesn't expose a per-branch force
// push (the project package's PushForce did per-branch; here
// we go through the all-refs entry point — for a fresh project
// on main only, the effect is the same).
func TestPushForceOverwritesDivergedRemoteIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	// Client A writes and pushes normally to bare.
	wsA, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wfA, err := wsA.ForProject(60, bare)
	if err != nil {
		t.Fatalf("clone A: %v", err)
	}
	if _, err := wfA.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:a",
		BranchOverride: "main",
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "a"), "result.md"), Content: []byte("alice v1")},
		},
	}); err != nil {
		t.Fatalf("A submit: %v", err)
	}

	// Client B starts on an unrelated bare (same seed shape,
	// different history). Submit locally so HEAD is a real commit.
	bareB := initBareForWorkspaceTest(t)
	wsB, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wfB, err := wsB.ForProject(60, bareB)
	if err != nil {
		t.Fatalf("clone B: %v", err)
	}
	if _, err := wfB.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:b",
		BranchOverride: "main",
		Citizen:        Identity{Name: "bob", Email: "bob@example.com"},
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "b"), "result.md"), Content: []byte("bob v1")},
		},
	}); err != nil {
		t.Fatalf("B initial submit: %v", err)
	}

	// Repoint B's clone at A's bare via the Ops surface
	// (RemoveOrigin + EnsureOrigin). Workflow exposes those
	// primitives so this scenario doesn't need to bypass
	// the seam.
	if err := wfB.git.RemoveOrigin(); err != nil {
		t.Fatalf("remove origin: %v", err)
	}
	if err := wfB.git.EnsureOrigin(bare); err != nil {
		t.Fatalf("set origin to bare: %v", err)
	}

	// Normal push (force=false) should fail against the
	// divergent remote. Force push wins.
	if err := wfB.git.PushAllRefs(false); err == nil {
		t.Fatal("expected normal Push to fail against diverged remote")
	}
	if err := wfB.git.PushAllRefs(true); err != nil {
		t.Fatalf("PushAllRefs(force=true): %v", err)
	}

	verifyDir := t.TempDir()
	if _, err := gogit.PlainClone(verifyDir, false, &gogit.CloneOptions{URL: bare}); err != nil {
		t.Fatalf("verify clone: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, resolveTestResultDir(1, "", "a"), "result.md")); !os.IsNotExist(err) {
		t.Errorf("expected A's file to be gone after force push, stat err: %v", err)
	}
	if _, err := os.Stat(filepath.Join(verifyDir, resolveTestResultDir(1, "", "b"), "result.md")); err != nil {
		t.Errorf("expected B's file on remote after force push: %v", err)
	}
}

// TestSubmitTaskResult_FailsClearlyAgainstUnreachableRemoteIntegration
// verifies that a non-recoverable push failure (bogus remote)
// surfaces a clean error naming the actual failure (push) and
// carrying the underlying reason, without retrying uselessly.
//
// A missing-repository error is NOT non-FF, so SubmitTaskResult
// returns immediately with a clear message instead of looping.
func TestSubmitTaskResult_FailsClearlyAgainstUnreachableRemoteIntegration(t *testing.T) {
	bare := initBareForWorkspaceTest(t)

	ws, _ := NewWorkspace(t.TempDir(), NewProductionConventions(), WithLogger(nullLogger()))
	wf, err := ws.ForProject(61, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Point the project at a bogus remote so push fails with
	// "repository not found" — a hard error the retry loop
	// cannot recover from.
	if err := wf.git.RemoveOrigin(); err != nil {
		t.Fatalf("remove origin: %v", err)
	}
	bogus := filepath.Join(t.TempDir(), "nonexistent.git")
	if err := wf.git.EnsureOrigin(bogus); err != nil {
		t.Fatalf("set origin to bogus: %v", err)
	}

	_, err = wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "1:1:x",
		BranchOverride: "main",
		MaxRetries:     2,
		Citizen:        Identity{Name: "alice", Email: "alice@example.com"},
		Files: []FileWrite{
			{RepoRelPath: filepath.Join(resolveTestResultDir(1, "", "x"), "result.md"), Content: []byte("data")},
		},
	})
	if err == nil {
		t.Fatal("expected submit to fail against bogus remote")
	}
	msg := err.Error()
	// The error must surface the push step and the underlying
	// repository-not-found reason so users can diagnose. Exact
	// wording differs between project package's old format and
	// enjugit's op-trace format, but both invariants need to hold:
	// "push" appears in the failed step name, and the underlying
	// "not found" surfaces somewhere in the error text.
	if !strings.Contains(msg, "push") {
		t.Errorf("expected error to mention push step, got: %q", msg)
	}
	if !strings.Contains(msg, "not found") {
		t.Errorf("expected underlying 'not found' reason in error, got: %q", msg)
	}
}
