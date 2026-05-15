package enjugit

import (
	"errors"
	"strings"
	"testing"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

func TestSubmitTaskResult_HappyPath(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Pre-stage: branch exists locally (daemon called startIterationBranch).
	fake.resolveMap["refs/heads/1-build/dev_a/iter-1"] = "currenttip"

	req := SubmitRequest{
		TaskID:    "7:1:dev_a",
		IterSeq:   1,
		RunSeq:    1,
		RunSlug:   "build",
		TaskDef:   "dev_a",
		RunBranch: "main",
		Files: []FileWrite{
			{RepoRelPath: "out/result.md", Content: []byte("done")},
		},
		Citizen:   Identity{Name: "Alice", Email: "alice@x"},
		ModelName: "claude-3.5-sonnet",
	}
	res, err := wf.SubmitTaskResult(req)
	if err != nil {
		t.Fatalf("SubmitTaskResult: %v", err)
	}
	if res.BranchName != "1-build/dev_a/iter-1" {
		t.Errorf("BranchName: got %q", res.BranchName)
	}
	if res.CommitSHA == "" {
		t.Error("CommitSHA empty")
	}
	// Atomic: WithLock invoked once enclosing all ops.
	if fake.callCount("WithLock") != 1 {
		t.Errorf("expected 1 WithLock, got %d", fake.callCount("WithLock"))
	}
	// Verified push.
	pv := fake.lastCall("PushWithVerify")
	if pv == nil {
		t.Fatal("PushWithVerify not called")
	}
	if pv.Args[0] != "1-build/dev_a/iter-1" {
		t.Errorf("PushWithVerify branch: got %v", pv.Args[0])
	}
}

func TestSubmitTaskResult_ComposesTrailers(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/1-r/dev/iter-2"] = "x"

	req := SubmitRequest{
		TaskID:    "7:1:dev",
		IterSeq:   2,
		RunSeq:    1,
		RunSlug:   "r",
		TaskDef:   "dev",
		Files:     []FileWrite{{RepoRelPath: "x", Content: []byte("y")}},
		Citizen:   Identity{Name: "Bot", Email: "bot@x"},
		ModelName: "claude-3.5-sonnet",
		Verdict:   "approve",
	}
	if _, err := wf.SubmitTaskResult(req); err != nil {
		t.Fatal(err)
	}
	commit := fake.lastCall("CommitFiles")
	cr := commit.Args[0].(git.CommitRequest)
	msg := cr.Message
	for _, expected := range []string{
		"Enju-Task-Complete: 7:1:dev",
		"Enju-Iter-Seq: 2",
		"Enju-Verdict: approve",
		"AI-Model: claude-3.5-sonnet",
		"Co-Authored-By: Claude (claude-3.5-sonnet) <noreply@anthropic.com>",
	} {
		if !strings.Contains(msg, expected) {
			t.Errorf("commit message missing %q\n%s", expected, msg)
		}
	}
}

func TestSubmitTaskResult_EmptyTaskID(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.SubmitTaskResult(SubmitRequest{
		Files: []FileWrite{{RepoRelPath: "x", Content: []byte("y")}},
	})
	if err == nil {
		t.Error("expected error for empty TaskID")
	}
}

func TestSubmitTaskResult_NoFiles(t *testing.T) {
	wf, _ := makeWorkflow(t)
	_, err := wf.SubmitTaskResult(SubmitRequest{TaskID: "x"})
	if err == nil {
		t.Error("expected error for empty Files")
	}
}

func TestSubmitTaskResult_PushVerifyFailedTranslated(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/1-r/x/iter-1"] = "old"
	fake.inject("PushWithVerify", &git.ErrVerifyFailed{
		Branch: "1-r/x/iter-1", LocalSHA: "loc", RemoteSHA: "rem",
	})

	_, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:  "7:1:x",
		IterSeq: 1,
		RunSeq:  1,
		RunSlug: "r",
		TaskDef: "x",
		Files:   []FileWrite{{RepoRelPath: "x", Content: []byte("y")}},
	})
	if !errors.Is(err, ErrSubmitVerifyFailed) {
		t.Errorf("expected ErrSubmitVerifyFailed, got %v", err)
	}
	var verr *ErrSubmitVerify
	if !errors.As(err, &verr) {
		t.Fatal("expected *ErrSubmitVerify in chain")
	}
	if verr.LocalSHA != "loc" || verr.RemoteSHA != "rem" {
		t.Errorf("verify error context lost: %+v", verr)
	}
}

// TestSubmitTaskResult_TraceNarratesSteps locks the architectural
// promise: when SubmitTaskResult fails, the returned error
// carries a step-by-step trace. Operators can read which step
// succeeded, which failed, and why — without log archaeology.
func TestSubmitTaskResult_TraceNarratesSteps(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/1-r/dev/iter-1"] = "x"
	fake.inject("PushWithVerify", &git.ErrVerifyFailed{
		Branch: "1-r/dev/iter-1", LocalSHA: "loc", RemoteSHA: "rem",
	})

	_, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:  "7:1:dev",
		IterSeq: 1,
		RunSeq:  1,
		RunSlug: "r",
		TaskDef: "dev",
		Files:   []FileWrite{{RepoRelPath: "x", Content: []byte("y")}},
	})
	if err == nil {
		t.Fatal("expected error from injected push-verify failure")
	}
	var opErr *WorkflowOpError
	if !errors.As(err, &opErr) {
		t.Fatalf("expected *WorkflowOpError in chain, got %T: %v", err, err)
	}
	if opErr.Op != "SubmitTaskResult" {
		t.Errorf("Op: got %q, want SubmitTaskResult", opErr.Op)
	}
	if opErr.Context["task_id"] != "7:1:dev" {
		t.Errorf("Context[task_id]: got %q", opErr.Context["task_id"])
	}
	if opErr.Context["branch"] == "" {
		t.Error("Context[branch] should be populated")
	}
	// Steps must show prepare-branch ok, commit ok, push-verify failed.
	stepStatus := map[string]string{}
	for _, s := range opErr.Steps {
		stepStatus[s.Name] = s.Status
	}
	if stepStatus["prepare-branch"] != "ok" {
		t.Errorf("prepare-branch should be ok, got %q", stepStatus["prepare-branch"])
	}
	if stepStatus["commit"] != "ok" {
		t.Errorf("commit should be ok, got %q", stepStatus["commit"])
	}
	if stepStatus["push-verify"] != "failed" {
		t.Errorf("push-verify should be failed, got %q", stepStatus["push-verify"])
	}
	// errors.Is still routes correctly — the trace shape doesn't
	// break sentinel-based caller logic.
	if !errors.Is(err, ErrSubmitVerifyFailed) {
		t.Error("errors.Is(err, ErrSubmitVerifyFailed) should still be true")
	}
}

// TestAutoMergeAcceptedTopic_ChecksOutTargetWhenHeadElsewhere
// is the regression guard for the lost-commit bug — for the
// LLM/bot path, where HEAD enters the merge sitting on the
// topic branch. Without an explicit Checkout(target), HEAD
// would stay on topic after the FF and the next iteration's
// fork would miss any user commits made between submits.
func TestAutoMergeAcceptedTopic_ChecksOutTargetWhenHeadElsewhere(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// Simulate the LLM/bot entry shape: HEAD on the topic
	// branch, not on the merge target.
	fake.headBranch = "topic"
	_, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "x", AutoOrManual: "auto"})
	if err != nil {
		t.Fatalf("MergeAcceptedTopic: %v", err)
	}
	checkoutCall := fake.lastCall("Checkout")
	if checkoutCall == nil {
		t.Fatal("Checkout(target) not called — HEAD wouldn't end up on target")
	}
	if checkoutCall.Args[0] != "main" {
		t.Errorf("Checkout target: got %v, want main", checkoutCall.Args[0])
	}
}

// TestAutoMergeAcceptedTopic_SkipsCheckoutWhenHeadAlreadyOnTarget
// is the Phase 1.4 companion: when HEAD enters the merge already
// on target (compute path — pre-claim CheckoutBranch left HEAD
// on the run branch and the no-checkout PlumbingCommit didn't
// move it), the post-FF Checkout becomes redundant AND harmful
// (triggers preserve-restore on a worktree that holds untracked
// script outputs). Skip it.
func TestAutoMergeAcceptedTopic_SkipsCheckoutWhenHeadAlreadyOnTarget(t *testing.T) {
	wf, fake := makeWorkflow(t)
	// fake.headBranch defaults to "main" — same as our merge target.
	_, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "x", AutoOrManual: "auto"})
	if err != nil {
		t.Fatalf("MergeAcceptedTopic: %v", err)
	}
	if call := fake.lastCall("Checkout"); call != nil {
		t.Errorf("expected Checkout to be skipped when HEAD already on target, got call %+v", call)
	}
}

// TestAutoMergeAcceptedTopic_TraceNarrates locks the same
// architectural promise for the merge path: when a merge fails
// with a conflict, the trace tells the operator "we tried FF,
// it returned non-FF, then we tried merge-commit which conflicted
// on these paths" — instead of just an opaque ErrMergeConflict.
func TestAutoMergeAcceptedTopic_TraceNarrates(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)
	fake.inject("MergeWithCommit", &git.ErrConflict{Paths: []string{"a.go"}})

	_, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "x", AutoOrManual: "auto"})
	if err == nil {
		t.Fatal("expected error from injected conflict")
	}
	var opErr *WorkflowOpError
	if !errors.As(err, &opErr) {
		t.Fatalf("expected *WorkflowOpError, got %T: %v", err, err)
	}
	stepStatus := map[string]string{}
	for _, s := range opErr.Steps {
		stepStatus[s.Name] = s.Status
	}
	// The trace must show: FF was tried (skipped because non-FF),
	// merge-commit was tried (failed because conflict).
	if stepStatus["merge-ff"] != "skipped" {
		t.Errorf("merge-ff should be skipped (non-FF triggers fallback), got %q", stepStatus["merge-ff"])
	}
	if stepStatus["merge-commit"] != "failed" {
		t.Errorf("merge-commit should be failed, got %q", stepStatus["merge-commit"])
	}
	// errors.Is still routes to the typed sentinel.
	if !errors.Is(err, ErrMergeConflict) {
		t.Error("errors.Is(err, ErrMergeConflict) should still be true")
	}
}

func TestAutoMergeAcceptedTopic_FastForward(t *testing.T) {
	wf, fake := makeWorkflow(t)

	res, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "7:1:trigger", AutoOrManual: "auto"})
	if err != nil {
		t.Fatalf("MergeAcceptedTopic: %v", err)
	}
	if res.NewTip != "ffsha" {
		t.Errorf("expected FF tip, got %q", res.NewTip)
	}
	if !res.FastForwarded {
		t.Errorf("expected FastForwarded=true on FF path")
	}
	// FF path used MergeFFOrFail then Push, no MergeWithCommit.
	if fake.callCount("MergeFFOrFail") != 1 {
		t.Errorf("expected 1 MergeFFOrFail, got %d", fake.callCount("MergeFFOrFail"))
	}
	if fake.callCount("MergeWithCommit") != 0 {
		t.Errorf("expected 0 MergeWithCommit on FF path, got %d", fake.callCount("MergeWithCommit"))
	}
	if fake.callCount("Push") != 1 {
		t.Errorf("expected 1 Push, got %d", fake.callCount("Push"))
	}
}

// TestAutoMergeAcceptedTopic_FFWorktreeSyncRouting pins the
// load-bearing three-way switch in the FF / HEAD-already-on-target
// branch: which worktree-sync primitive fires depends solely on
// the pre-merge targetSHA relative to the post-FF NewTip
// ("ffsha", from fakeOps.MergeFFOrFail). headBranch defaults to
// "main" == merge target, so every case enters the skip-branch.
//
// This is the routing the phantom-delete fix hinges on; the
// gitcli unit tests prove each primitive in isolation, this
// proves the wiring picks the right one.
func TestAutoMergeAcceptedTopic_FFWorktreeSyncRouting(t *testing.T) {
	cases := []struct {
		name        string
		targetSHA   string // resolveMap["main"]; "" = unset → ErrRefNotFound
		wantCall    string
		notExpected []string
	}{
		{
			name:        "normal FF: targetSHA != NewTip → FastForwardWorktree",
			targetSHA:   "oldmain",
			wantCall:    "FastForwardWorktree",
			notExpected: []string{"ReconcileWorktreeToHead", "SyncIndexToHead"},
		},
		{
			name:        "ref already at tip: targetSHA == NewTip → ReconcileWorktreeToHead",
			targetSHA:   "ffsha",
			wantCall:    "ReconcileWorktreeToHead",
			notExpected: []string{"FastForwardWorktree", "SyncIndexToHead"},
		},
		{
			name:        "degraded: targetSHA unresolved → SyncIndexToHead",
			targetSHA:   "",
			wantCall:    "SyncIndexToHead",
			notExpected: []string{"FastForwardWorktree", "ReconcileWorktreeToHead"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wf, fake := makeWorkflow(t)
			if tc.targetSHA != "" {
				fake.resolveMap["main"] = tc.targetSHA
			} // else: leave unset → ResolveRef("main") errors → targetSHA==""
			if _, err := wf.MergeAcceptedTopic("topic", "main",
				MergeAuthor{TaskID: "x", AutoOrManual: "auto"}); err != nil {
				t.Fatalf("MergeAcceptedTopic: %v", err)
			}
			if n := fake.callCount(tc.wantCall); n != 1 {
				t.Errorf("expected exactly 1 %s call, got %d", tc.wantCall, n)
			}
			for _, m := range tc.notExpected {
				if n := fake.callCount(m); n != 0 {
					t.Errorf("expected 0 %s calls, got %d", m, n)
				}
			}
		})
	}
}

// TestAutoMergeAcceptedTopic_NoOriginSkipsFetchAndPush pins the
// post-Phase-8 single-store contract: when the workflow's clone has
// no origin configured, the merge's Fetch + Push steps are skipped
// rather than failing the merge. The local FF/merge-commit IS the
// final state; there's no remote to sync with.
//
// Pre-fix, every merge in a no-origin project failed because git's
// Push surfaced "fatal: 'origin' does not appear to be a git
// repository", which the wrapper translated into a task failure.
func TestAutoMergeAcceptedTopic_NoOriginSkipsFetchAndPush(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.remoteURL = "" // no origin configured

	res, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "7:1:trigger", AutoOrManual: "auto"})
	if err != nil {
		t.Fatalf("MergeAcceptedTopic should succeed without origin: %v", err)
	}
	if !res.FastForwarded {
		t.Errorf("FF still expected even without origin (it's a local ref op)")
	}
	if fake.callCount("Fetch") != 0 {
		t.Errorf("Fetch should be skipped when no origin, got %d calls", fake.callCount("Fetch"))
	}
	if fake.callCount("Push") != 0 {
		t.Errorf("Push should be skipped when no origin, got %d calls", fake.callCount("Push"))
	}
	if fake.callCount("MergeFFOrFail") != 1 {
		t.Errorf("expected the local FF op to still fire, got %d MergeFFOrFail calls",
			fake.callCount("MergeFFOrFail"))
	}
}

func TestAutoMergeAcceptedTopic_NonFFFallsBackToMergeCommit(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)

	res, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "7:1:trigger", AutoOrManual: "auto"})
	if err != nil {
		t.Fatalf("MergeAcceptedTopic: %v", err)
	}
	if res.NewTip != "mergesha" {
		t.Errorf("expected merge-commit tip, got %q", res.NewTip)
	}
	if res.FastForwarded {
		t.Errorf("expected FastForwarded=false on non-FF path")
	}
	if fake.callCount("MergeWithCommit") != 1 {
		t.Errorf("expected 1 MergeWithCommit on non-FF, got %d", fake.callCount("MergeWithCommit"))
	}
}

func TestAutoMergeAcceptedTopic_ConflictTranslated(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)
	fake.inject("MergeWithCommit", &git.ErrConflict{
		Paths: []string{"src/foo.go", "src/bar.go"},
	})

	_, err := wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "7:1:trigger", AutoOrManual: "auto"})
	if !errors.Is(err, ErrMergeConflict) {
		t.Fatalf("expected ErrMergeConflict, got %v", err)
	}
	var conflict *ErrConflict
	if !errors.As(err, &conflict) {
		t.Fatal("expected *ErrConflict in chain")
	}
	if len(conflict.Paths) != 2 {
		t.Errorf("expected 2 conflict paths, got %d", len(conflict.Paths))
	}
}

func TestAutoMergeAcceptedTopic_AutoUsesSystemAuthor(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)

	wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{TaskID: "x", AutoOrManual: "auto"})

	mc := fake.lastCall("MergeWithCommit")
	if mc.Args[3] != "Enju System" {
		t.Errorf("auto merge author: got %v, want 'Enju System'", mc.Args[3])
	}
}

func TestAutoMergeAcceptedTopic_ManualUsesCitizen(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.inject("MergeFFOrFail", git.ErrPushNonFF)

	wf.MergeAcceptedTopic("topic", "main",
		MergeAuthor{
			TaskID:       "x",
			AutoOrManual: "manual",
			Citizen:      Identity{Name: "Carol", Email: "c@x"},
		})

	mc := fake.lastCall("MergeWithCommit")
	if mc.Args[3] != "Carol" {
		t.Errorf("manual merge author: got %v, want 'Carol'", mc.Args[3])
	}
}

func TestComposeCommitMessage_TrailerOrder(t *testing.T) {
	convs := NewProductionConventions()
	msg := composeCommitMessage(convs, "subj", "body", map[string]string{
		"AI-Model":      "claude",
		"Enju-Task-Complete":  "x",
		"Enju-Iter-Seq": "1",
		"Custom-Trail":  "v",
	})
	// Enju-Task-Complete must come before AI-Model per ProductionTrailerOrder.
	taskIdx := strings.Index(msg, "Enju-Task-Complete:")
	aiIdx := strings.Index(msg, "AI-Model:")
	if taskIdx == -1 || aiIdx == -1 {
		t.Fatalf("expected both trailers, got:\n%s", msg)
	}
	if taskIdx >= aiIdx {
		t.Errorf("Enju-Task-Complete must precede AI-Model:\n%s", msg)
	}
	if !strings.Contains(msg, "Custom-Trail: v") {
		t.Errorf("custom trailer missing:\n%s", msg)
	}
}

func TestComposeCommitMessage_SkipsEmpty(t *testing.T) {
	convs := NewProductionConventions()
	msg := composeCommitMessage(convs, "subj", "", map[string]string{
		"Enju-Task-Complete": "x",
		"AI-Model":     "", // must be skipped
	})
	if strings.Contains(msg, "AI-Model:") {
		t.Errorf("empty trailer should be skipped:\n%s", msg)
	}
}

func TestFetchAllRefs(t *testing.T) {
	wf, fake := makeWorkflow(t)
	if err := wf.FetchAllRefs(); err != nil {
		t.Fatalf("FetchAllRefs: %v", err)
	}
	if fake.callCount("Fetch") != 1 {
		t.Errorf("expected 1 Fetch, got %d", fake.callCount("Fetch"))
	}
}

// TestFetchAllRefs_NoOriginIsNoOp pins the post-Phase-8 single-store
// contract: the bot daemon's pre-claim FetchAllRefs is a no-op when
// origin isn't configured. Pre-fix it surfaced "fatal: 'origin' does
// not appear to be a git repository" as a hard error from the bot's
// ERROR log, scaring operators into thinking the project was broken.
func TestFetchAllRefs_NoOriginIsNoOp(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.remoteURL = ""
	if err := wf.FetchAllRefs(); err != nil {
		t.Fatalf("FetchAllRefs without origin should not error: %v", err)
	}
	if fake.callCount("Fetch") != 0 {
		t.Errorf("Fetch should be skipped when no origin, got %d calls",
			fake.callCount("Fetch"))
	}
}

// TestPullBranch_NoOriginIsNoOp pins the same contract for
// PullBranch. This is the ls-remote-origin call that the bot
// daemon's pre-claim reconcile hits via service.PullBranchWithReconcileWF.
func TestPullBranch_NoOriginIsNoOp(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.remoteURL = ""
	if err := wf.PullBranch("main"); err != nil {
		t.Fatalf("PullBranch without origin should not error: %v", err)
	}
	if fake.callCount("PullBranch") != 0 {
		t.Errorf("PullBranch should be skipped when no origin, got %d calls",
			fake.callCount("PullBranch"))
	}
}

// TestFetchBranch_NoOriginIsNoOp mirrors the pair above for the
// branch-specific fetch entry point.
func TestFetchBranch_NoOriginIsNoOp(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.remoteURL = ""
	if err := wf.FetchBranch("main"); err != nil {
		t.Fatalf("FetchBranch without origin should not error: %v", err)
	}
	if fake.callCount("FetchBranch") != 0 {
		t.Errorf("FetchBranch should be skipped when no origin, got %d calls",
			fake.callCount("FetchBranch"))
	}
}

func TestReadFileAtCommit_Passthrough(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.readContent["sha:path"] = []byte("hello")
	body, found, err := wf.ReadFileAtCommit("sha", "path")
	if err != nil {
		t.Fatalf("ReadFileAtCommit: %v", err)
	}
	if !found || string(body) != "hello" {
		t.Errorf("got %q found=%v", body, found)
	}
}
