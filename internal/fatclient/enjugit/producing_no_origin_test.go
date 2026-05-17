package enjugit

// No-origin reproducers for the *porcelain* submit path.
//
// Background: when origin is unset (a solo single-machine
// project with no remote — first-class since Phase 8 dropped the
// managed bare), the trailing push step must be skipped, not
// attempted. The local commit already landed in the single
// store; there is nothing to push to.
//
// SubmitComputeTaskResult already has this gate and a regression
// test (TestSubmitComputeTaskResult_NoOriginSkipsPush). The gate
// was never mirrored onto the sibling submit paths, so the same
// no-origin scenario was never asserted for SubmitTaskResult (the
// MCP / enju_submit_result path) or SubmitBatch — which is why a
// fully green suite still shipped the leak. These tests close that
// gap: they reproduce the failure today (push to a nonexistent
// origin → push-verify fails → submit rolls back) and pin the
// correct behavior (push silently skipped, commit lands locally)
// for when the guard is applied.

import "testing"

// TestSubmitTaskResult_NoOriginSkipsPush is the porcelain-path
// mirror of TestSubmitComputeTaskResult_NoOriginSkipsPush. It
// reproduces the exact failure hit via enju_submit_result on a
// path-mode project with no remote: SubmitTaskResult's push-verify
// step is unguarded, so it tries to push to a nonexistent origin
// and the whole submit fails / rolls back, leaving the task stuck
// in `claimed`. Once SubmitTaskResult guards on RemoteURL()==""
// (mirroring SubmitComputeTaskResult), this goes green.
func TestSubmitTaskResult_NoOriginSkipsPush(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 44)
	wf, err := ws.ForProject(44, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	// Drop origin so the push step has no remote to target —
	// the production shape is a solo single-machine project with
	// no GitHub remote configured.
	if err := wf.git.RemoveOrigin(); err != nil {
		t.Fatalf("RemoveOrigin: %v", err)
	}

	resultDir := resolveTestResultDir(1, "", "solo")
	res, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "44:1:solo",
		BranchOverride: "main",
		Citizen:        Identity{Name: "tamer", Email: "tamer@example.com"},
		Files: []FileWrite{
			{RepoRelPath: resultDir + "/result.md", Content: []byte("alone but committed\n")},
		},
	})
	if err != nil {
		t.Fatalf("SubmitTaskResult without origin must succeed (push is a no-op): %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("empty commit SHA")
	}

	// The load-bearing assertion: the commit landed locally on
	// the branch. Sharing via push is a separate concern that
	// only applies when origin points at a real remote — cloning
	// the bare to verify is impossible here (origin is gone) and
	// also not the point.
	localSHA, err := wf.git.LocalBranchHash("main")
	if err != nil {
		t.Fatalf("LocalBranchHash(main): %v", err)
	}
	if localSHA != res.CommitSHA {
		t.Errorf("local branch ref: got %s, want %s", localSHA, res.CommitSHA)
	}
}
