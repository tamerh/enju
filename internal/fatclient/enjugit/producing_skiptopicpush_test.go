package enjugit

// SkipTopicPush gate — the per-project push_topic_branches=false
// lever for solo bulk-data pipelines. Mirrors
// TestSubmitComputeTaskResult_HappyPath's setup, but flips
// SkipTopicPush=true and inverts the bare-side assertion:
//
//   - Local commit MUST still land on the topic branch (the
//     accept path reads from the local clone; auto-merge into
//     the run branch still works).
//   - Topic ref MUST NOT appear on the bare (origin) — that's
//     the entire point: avoid 100K × N litter ref noise at
//     scale.
//
// Without the gate, the trailing push-verify step in
// producing_plumbing.go would push the topic ref to origin
// regardless. With the gate, push-verify is skipped and the topic
// stays local.

import "testing"

func TestSubmitComputeTaskResult_SkipTopicPushKeepsLocalSilent(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 142)
	wf, err := ws.ForProject(142, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	res, err := wf.SubmitComputeTaskResult(SubmitRequest{
		TaskID:        "142:1:fetch_quiet",
		IterSeq:       1,
		RunSeq:        1,
		RunSlug:       "quiet-load",
		TaskDef:       "fetch_quiet",
		RunBranch:     "main",
		Citizen:       Identity{Name: "tamer", Email: "tamer@example.com"},
		SkipTopicPush: true,
		Files: []FileWrite{
			{
				RepoRelPath: "data/fetch_quiet/result.md",
				Content:     []byte("quietly committed\n"),
			},
		},
	})
	if err != nil {
		t.Fatalf("SubmitComputeTaskResult: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("empty commit SHA — local commit must still land even with SkipTopicPush")
	}

	expectedBranch := wf.convs.BranchName(1, "quiet-load", "fetch_quiet", "", 1)
	if res.BranchName != expectedBranch {
		t.Errorf("BranchName: got %s, want %s", res.BranchName, expectedBranch)
	}

	// Load-bearing assertion #1: the topic-ref MUST NOT exist on
	// the bare. RemoteBranchHash returns "" for a branch the
	// remote doesn't carry — that's the success case here.
	remoteSHA, err := wf.git.RemoteBranchHash(expectedBranch)
	if err != nil {
		t.Fatalf("RemoteBranchHash: %v", err)
	}
	if remoteSHA != "" {
		t.Errorf("topic branch %s leaked to origin (SHA %s) — SkipTopicPush gate is not honored",
			expectedBranch, remoteSHA)
	}

	// Load-bearing assertion #2: the commit DID land locally on
	// the topic ref. Auto-merge / coord acceptance reads from
	// here; if this is missing the run is stuck.
	localSHA, err := wf.git.LocalBranchHash(expectedBranch)
	if err != nil {
		t.Fatalf("LocalBranchHash(%s): %v", expectedBranch, err)
	}
	if localSHA != res.CommitSHA {
		t.Errorf("local topic ref: got %s, want %s", localSHA, res.CommitSHA)
	}
}

// TestSubmitTaskResult_SkipTopicPushKeepsLocalSilent is the
// porcelain-path counterpart: SubmitTaskResult is the LLM/bot
// submit path, which has its own (independent) gating site in
// producing.go. Same contract — commit lands locally, no push.
func TestSubmitTaskResult_SkipTopicPushKeepsLocalSilent(t *testing.T) {
	bare := initBareForWorkspaceTest(t)
	ws, _ := newWorkspaceForIDs(t, 143)
	wf, err := ws.ForProject(143, bare)
	if err != nil {
		t.Fatalf("ForProject: %v", err)
	}

	const topic = "143-quiet-load/llm_step/iter-1"
	resultDir := resolveTestResultDir(1, "", "llm_step")
	res, err := wf.SubmitTaskResult(SubmitRequest{
		TaskID:         "143:1:llm_step",
		BranchOverride: topic,
		RunBranch:      "main",
		Citizen:        Identity{Name: "tamer", Email: "tamer@example.com"},
		SkipTopicPush:  true,
		Files: []FileWrite{
			{RepoRelPath: resultDir + "/result.md", Content: []byte("quiet llm\n")},
		},
	})
	if err != nil {
		t.Fatalf("SubmitTaskResult: %v", err)
	}
	if res.CommitSHA == "" {
		t.Fatal("empty commit SHA — local commit must still land")
	}

	remoteSHA, err := wf.git.RemoteBranchHash(topic)
	if err != nil {
		t.Fatalf("RemoteBranchHash: %v", err)
	}
	if remoteSHA != "" {
		t.Errorf("topic %s leaked to origin (SHA %s) — porcelain gate not honored",
			topic, remoteSHA)
	}
	localSHA, err := wf.git.LocalBranchHash(topic)
	if err != nil {
		t.Fatalf("LocalBranchHash(%s): %v", topic, err)
	}
	if localSHA != res.CommitSHA {
		t.Errorf("local topic ref: got %s, want %s", localSHA, res.CommitSHA)
	}
}
