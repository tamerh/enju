package enjugit

// Tests for Workflow.PublishRunArtifacts — the run-completion publish
// that lays the run's declared output set onto the base (deliverable)
// branch and, in push mode, shares exactly { base, run branch } (and
// topic branches only when opted in). The base branch must never
// receive enju's .enju/ provenance trail.

import (
	"errors"
	"testing"

	git "github.com/enju-ai/enju/internal/fatclient/enjugit/internal/gitcli"
)

// publishHarness wires a Workflow whose base-branch prepare hits the
// local-checkout path and whose run branch resolves to a tip SHA.
func publishHarness(t *testing.T) (*Workflow, *fakeOps, string) {
	t.Helper()
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "mainsha00000000000000000000000000000000"
	runTip := "runtip0000000000000000000000000000000000"
	fake.resolveMap["run-1"] = runTip
	return wf, fake, runTip
}

func lastCommitReq(t *testing.T, fake *fakeOps) git.CommitRequest {
	t.Helper()
	c := fake.lastCall("CommitFiles")
	if c == nil {
		t.Fatal("CommitFiles was not called")
	}
	req, ok := c.Args[0].(git.CommitRequest)
	if !ok {
		t.Fatalf("CommitFiles arg is %T, want git.CommitRequest", c.Args[0])
	}
	return req
}

func pushedRefs(fake *fakeOps) []string {
	var refs []string
	for _, c := range fake.calls {
		if c.Method == "Push" {
			refs = append(refs, c.Args[0].(string))
		}
	}
	return refs
}

// TestPublishRunArtifacts_ArtifactsOnlyNoEnju — only the declared
// non-.enju/ paths are read from the run tip and committed to base;
// a .enju/ path that somehow reaches the request is filtered out
// (defense in depth — the coordinator already excludes it).
func TestPublishRunArtifacts_ArtifactsOnlyNoEnju(t *testing.T) {
	wf, fake, runTip := publishHarness(t)
	fake.readContent[runTip+":results/out.csv"] = []byte("rows")
	fake.readContent[runTip+":.enju/runs/3/build/result.md"] = []byte("provenance")

	res, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch:  "run-1",
		BaseBranch: "main",
		Paths:      []string{"results/out.csv", ".enju/runs/3/build/result.md"},
		Author:     systemAuthor,
	})
	if err != nil {
		t.Fatalf("PublishRunArtifacts: %v", err)
	}
	if res.NoOp {
		t.Error("expected a real publish commit, got NoOp")
	}
	req := lastCommitReq(t, fake)
	if len(req.Files) != 1 || req.Files[0].RepoRelPath != "results/out.csv" {
		t.Errorf("committed files = %+v, want only results/out.csv (.enju/ filtered)", req.Files)
	}
	if len(req.StagePaths) != 1 || req.StagePaths[0] != "results/out.csv" {
		t.Errorf("staged paths = %v, want [results/out.csv]", req.StagePaths)
	}
	if string(req.Files[0].Content) != "rows" {
		t.Errorf("content not read from run tip: got %q", req.Files[0].Content)
	}
	if n := fake.callCount("Push"); n != 0 {
		t.Errorf("merge-default (no Push flag): expected 0 Push, got %d", n)
	}
}

// TestPublishRunArtifacts_MissingRunBranch — run branch can't be
// resolved → hard error, base untouched.
func TestPublishRunArtifacts_MissingRunBranch(t *testing.T) {
	wf, fake := makeWorkflow(t)
	fake.resolveMap["refs/heads/main"] = "mainsha"
	// run-1 intentionally absent from resolveMap.

	_, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "run-1", BaseBranch: "main",
		Paths: []string{"out.txt"}, Author: systemAuthor,
	})
	if err == nil {
		t.Fatal("expected error when run branch can't be resolved")
	}
	if fake.callCount("CommitFiles") != 0 {
		t.Error("CommitFiles must not run when run tip resolve fails")
	}
}

// TestPublishRunArtifacts_PushSharesBaseAndRun — push mode pushes
// exactly { base, run branch }; topic branches stay local by default.
func TestPublishRunArtifacts_PushSharesBaseAndRun(t *testing.T) {
	wf, fake, runTip := publishHarness(t)
	fake.readContent[runTip+":out.txt"] = []byte("x")
	fake.branches = []string{"main", "run-1", "run-1/build/iter-1", "run-2"}

	res, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "run-1", BaseBranch: "main",
		Paths: []string{"out.txt"}, Author: systemAuthor,
		Push: true,
	})
	if err != nil {
		t.Fatalf("PublishRunArtifacts: %v", err)
	}
	got := pushedRefs(fake)
	if len(got) != 2 || got[0] != "main" || got[1] != "run-1" {
		t.Errorf("pushed refs = %v, want exactly [main run-1]", got)
	}
	for _, r := range got {
		if r == "run-1/build/iter-1" || r == "run-2" {
			t.Errorf("topic / unrelated branch %q must not be pushed by default", r)
		}
	}
	if len(res.Pushed) != 2 {
		t.Errorf("result.Pushed = %v, want 2 entries", res.Pushed)
	}
}

// TestPublishRunArtifacts_NoDeclaredFilesNoOp — a citizen-content-
// only run declares no file outputs: no publish commit is made, but
// in push mode { base, run branch } are still shared (the run branch
// is the deliverable+audit).
func TestPublishRunArtifacts_NoDeclaredFilesNoOp(t *testing.T) {
	wf, fake, _ := publishHarness(t)
	fake.branches = []string{"main", "run-1"}

	res, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "run-1", BaseBranch: "main",
		Paths: nil, Author: systemAuthor,
		Push: true,
	})
	if err != nil {
		t.Fatalf("PublishRunArtifacts: %v", err)
	}
	if !res.NoOp {
		t.Error("expected NoOp=true when no declared file outputs")
	}
	if fake.callCount("CommitFiles") != 0 {
		t.Error("CommitFiles must not run with no declared files")
	}
	got := pushedRefs(fake)
	if len(got) != 2 || got[0] != "main" || got[1] != "run-1" {
		t.Errorf("pushed refs = %v, want [main run-1] even for a no-file run", got)
	}
}

// TestPublishRunArtifacts_MergeModeNoPush — Push=false never touches
// the remote, even with branches present.
func TestPublishRunArtifacts_MergeModeNoPush(t *testing.T) {
	wf, fake, runTip := publishHarness(t)
	fake.readContent[runTip+":out.txt"] = []byte("x")
	fake.branches = []string{"main", "run-1", "run-1/build/iter-1"}

	if _, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "run-1", BaseBranch: "main",
		Paths: []string{"out.txt"}, Author: systemAuthor,
	}); err != nil {
		t.Fatalf("PublishRunArtifacts: %v", err)
	}
	if n := fake.callCount("Push"); n != 0 {
		t.Errorf("merge mode must push nothing, got %d Push calls", n)
	}
	if fake.callCount("CommitFiles") != 1 {
		t.Errorf("expected 1 publish commit, got %d", fake.callCount("CommitFiles"))
	}
}

// TestPublishRunArtifacts_RequiresBothBranches — empty run or base
// branch is a programming error; returns without touching git.
func TestPublishRunArtifacts_RequiresBothBranches(t *testing.T) {
	wf, _ := makeWorkflow(t)
	if _, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "", BaseBranch: "main", Author: systemAuthor,
	}); err == nil {
		t.Error("expected error for empty RunBranch")
	}
	if _, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "run-1", BaseBranch: "", Author: systemAuthor,
	}); err == nil {
		t.Error("expected error for empty BaseBranch")
	}
}

// TestPublishRunArtifacts_PushBestEffortNonFatal — a push failure
// does not fail the verb (the local publish already landed); the
// failed ref is simply absent from result.Pushed.
func TestPublishRunArtifacts_PushBestEffortNonFatal(t *testing.T) {
	wf, fake, runTip := publishHarness(t)
	fake.readContent[runTip+":out.txt"] = []byte("x")
	fake.branches = []string{"main", "run-1"}
	fake.inject("Push", errors.New("remote rejected"))

	res, err := wf.PublishRunArtifacts(PublishRunArtifactsRequest{
		RunBranch: "run-1", BaseBranch: "main",
		Paths: []string{"out.txt"}, Author: systemAuthor,
		Push: true,
	})
	if err != nil {
		t.Fatalf("push failure must be non-fatal, got err: %v", err)
	}
	if fake.callCount("CommitFiles") != 1 {
		t.Error("publish commit should still have landed locally")
	}
	if len(res.Pushed) != 0 {
		t.Errorf("result.Pushed = %v, want empty (all pushes failed)", res.Pushed)
	}
}
