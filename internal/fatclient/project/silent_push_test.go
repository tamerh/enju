package project

// Push-verify regression tests. Production saw "commit-reported-
// to-coord-but-never-in-bare" failures across multiple iterations
// of the same task — pushBranchInternal returned success but the
// commit didn't reach the bare. The verify-after-push fix lists
// the remote ref after each push and confirms it equals the local
// commit; on mismatch it returns *ErrPushVerifyFailed which the
// fat-client surfaces as a push_verify_failed event.

import (
	"errors"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestPushVerify_HappyPathReachesBare is the baseline — a normal
// push to a real bare lands the commit and verify is silent.
// Regression guard against any future change that breaks the
// basic local-bare push contract.
func TestPushVerify_HappyPathReachesBare(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(7, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "7:1:t",
		Username: "developer-bot",
		Branch:   "topic-baseline",
		Files:    []FileWrite{{RepoRelPath: "out/baseline.md", Content: []byte("hi")}},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	if _, err := bareRepo.CommitObject(plumbing.NewHash(res.CommitSHA)); err != nil {
		t.Errorf("baseline broken: SubmitTaskResult returned SHA %s but bare doesn't have it: %v",
			res.CommitSHA, err)
	}
	ref, err := bareRepo.Reference(plumbing.NewBranchReferenceName("topic-baseline"), true)
	if err != nil {
		t.Fatalf("bare missing topic-baseline ref: %v", err)
	}
	if ref.Hash().String() != res.CommitSHA {
		t.Errorf("bare topic-baseline = %s, want fresh commit %s",
			ref.Hash(), res.CommitSHA)
	}
}

// TestPushVerify_DetectsRemoteRefDivergence pins the load-bearing
// case: simulate a silent-success failure mode by having the
// remote ref disagree with the local branch tip after push, then
// confirm verifyRemoteRefMatches returns *ErrPushVerifyFailed.
//
// Production trace: develop_config's submits returned SHAs but
// bare's topic branch ref stayed at seed. Verify catches that
// shape regardless of which underlying transport quirk caused
// it (NoErrAlreadyUpToDate, empty remote, wrong refspec, etc.).
func TestPushVerify_DetectsRemoteRefDivergence(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(11, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Land an initial commit on a topic branch via a real
	// submit. After this, bare's `topic` ref points at that
	// commit and proj's local branch ref does too.
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "11:1:t",
		Username: "developer-bot",
		Branch:   "topic-divergence",
		Files:    []FileWrite{{RepoRelPath: "out/v1.md", Content: []byte("v1")}},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("initial submit: %v", err)
	}
	originalSHA := plumbing.NewHash(res.CommitSHA)

	// Now manufacture the silent-success state: roll the bare's
	// branch ref backward to the seed, simulating "remote ref
	// didn't move even though we just pushed." Any go-git push
	// after this that returns NoErrAlreadyUpToDate (or any
	// other no-op) without actually advancing the remote tip
	// would leave the system in this exact state.
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	mainRef, err := bareRepo.Reference(plumbing.NewBranchReferenceName("main"), true)
	if err != nil {
		t.Fatalf("read main: %v", err)
	}
	seedSHA := mainRef.Hash()
	if err := bareRepo.Storer.SetReference(plumbing.NewHashReference(
		plumbing.NewBranchReferenceName("topic-divergence"), seedSHA,
	)); err != nil {
		t.Fatalf("force-reset bare topic-divergence: %v", err)
	}

	// Now ask the verify helper directly: local SHA is
	// originalSHA (where it was after submit), but the remote
	// has been moved backward.
	proj.Lock()
	vErr := proj.verifyRemoteRefMatches("topic-divergence", originalSHA)
	proj.Unlock()
	if vErr == nil {
		t.Fatal("expected ErrPushVerifyFailed when remote ref diverges from local tip; got nil")
	}
	var typed *ErrPushVerifyFailed
	if !errors.As(vErr, &typed) {
		t.Fatalf("expected *ErrPushVerifyFailed, got %T: %v", vErr, vErr)
	}
	if typed.LocalSHA != originalSHA.String() {
		t.Errorf("LocalSHA = %s, want %s", typed.LocalSHA, originalSHA)
	}
	if typed.RemoteSHA != seedSHA.String() {
		t.Errorf("RemoteSHA = %s, want seed %s", typed.RemoteSHA, seedSHA)
	}
	if typed.Branch != "topic-divergence" {
		t.Errorf("Branch = %q, want topic-divergence", typed.Branch)
	}
}

// TestPushVerify_DetectsMissingRemoteRef pins the "remote ref
// doesn't exist at all" case — push reported success but the
// branch never appeared on the remote (e.g., wrong refspec,
// transport ate the upload). Verify catches it and reports
// RemoteSHA as empty.
func TestPushVerify_DetectsMissingRemoteRef(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(12, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}

	// Make a local commit (via a normal submit so we have a
	// real SHA to verify against).
	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "12:1:t",
		Username: "developer-bot",
		Branch:   "topic-pre-vanish",
		Files:    []FileWrite{{RepoRelPath: "out/v.md", Content: []byte("v")}},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	localSHA := plumbing.NewHash(res.CommitSHA)

	// Vanish the remote ref entirely — simulates "push went
	// silently into the void, ref never created on the bare."
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	if err := bareRepo.Storer.RemoveReference(plumbing.NewBranchReferenceName("topic-pre-vanish")); err != nil {
		t.Fatalf("removing remote ref: %v", err)
	}

	proj.Lock()
	vErr := proj.verifyRemoteRefMatches("topic-pre-vanish", localSHA)
	proj.Unlock()
	if vErr == nil {
		t.Fatal("expected ErrPushVerifyFailed when remote ref is missing; got nil")
	}
	var typed *ErrPushVerifyFailed
	if !errors.As(vErr, &typed) {
		t.Fatalf("expected *ErrPushVerifyFailed, got %T: %v", vErr, vErr)
	}
	if typed.RemoteSHA != "" {
		t.Errorf("RemoteSHA = %q, want empty (missing) for a vanished ref", typed.RemoteSHA)
	}
	if typed.LocalSHA != localSHA.String() {
		t.Errorf("LocalSHA = %s, want %s", typed.LocalSHA, localSHA)
	}
}

// TestPushVerify_EmptyRemoteURL_NoOpDocumented pins an
// intentional gap: pushBranchInternal silently no-ops when
// remoteURL is empty, and verify is correspondingly skipped
// (there's nothing to verify against). This is by design for
// the operator-no-remote local-only mode.
//
// The bot path should NEVER have an empty remoteURL — its
// clone is sourced from either the project's real remote or
// the local managed bare. If a future regression lets a bot
// daemon's clone have empty remoteURL, that's an upstream bug
// (in OpenBotCloneAt / openOrClone / cache state) and not
// fixable here. This test documents the gap so a maintainer
// reading this file knows verify won't catch it.
func TestPushVerify_EmptyRemoteURL_NoOpDocumented(t *testing.T) {
	bare := initBareRemote(t)
	seedRemoteWithInitialCommit(t, bare)

	ws, err := NewOpener(t.TempDir(), nullLogger())
	if err != nil {
		t.Fatalf("opener: %v", err)
	}
	proj, err := ws.ForProject(42, bare)
	if err != nil {
		t.Fatalf("clone: %v", err)
	}
	// Force the empty-remoteURL state. (Reaching this state
	// in production would itself be a bug — see comment above.)
	proj.remoteURL = ""

	proj.Lock()
	res, err := proj.SubmitTaskResult(SubmitRequest{
		TaskID:   "42:1:t",
		Username: "developer-bot",
		Branch:   "topic-noremote",
		Files:    []FileWrite{{RepoRelPath: "out/x.md", Content: []byte("x")}},
	})
	proj.Unlock()
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("submit returned empty SHA")
	}

	// Documented current behavior: commit is local-only, bare
	// doesn't see it. Verify is skipped because remoteURL is
	// empty.
	bareRepo, err := gogit.PlainOpen(bare)
	if err != nil {
		t.Fatalf("open bare: %v", err)
	}
	if _, err := bareRepo.CommitObject(plumbing.NewHash(res.CommitSHA)); err == nil {
		t.Errorf("commit unexpectedly reached bare in empty-remoteURL mode — has the design changed? "+
			"If push now actually pushes when remoteURL is empty, update this test.")
	}
}
