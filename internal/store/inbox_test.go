package store

import (
	"strings"
	"testing"
	"time"
)

// TestListInbox_FiltersByAssignee pins the membership filter:
// a task assigned to "tamer" appears in tamer's inbox; tasks
// assigned to "alice" or unassigned tasks do not. The
// production assign_to shape is JSON-encoded (`["tamer"]`); the
// query uses json_each for membership.
func TestListInbox_FiltersByAssignee(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID

	now := time.Now()
	// Assigned to tamer — should appear.
	if err := s.CreateTask(&TaskRecord{
		ID: "tmine", RunID: runID, Seq: 1, TaskDefID: "tmine",
		Action: "review", ResultType: "text",
		State: TaskReady, Prompt: "review the draft",
		AssignTo:  `["tamer"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Assigned to alice — should NOT appear.
	if err := s.CreateTask(&TaskRecord{
		ID: "tothers", RunID: runID, Seq: 2, TaskDefID: "tothers",
		Action: "review", ResultType: "text",
		State: TaskReady, Prompt: "not your business",
		AssignTo:  `["alice"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Unassigned — should NOT appear.
	if err := s.CreateTask(&TaskRecord{
		ID: "topen", RunID: runID, Seq: 3, TaskDefID: "topen",
		Action: "answer", ResultType: "text",
		State: TaskReady, Prompt: "anyone can take this",
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Pending (not ready) — should NOT appear.
	if err := s.CreateTask(&TaskRecord{
		ID: "tlater", RunID: runID, Seq: 4, TaskDefID: "tlater",
		Action: "review", ResultType: "text",
		State: TaskPending, Prompt: "wait your turn",
		AssignTo:  `["tamer"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	items, err := s.ListInbox(projectID, "tamer")
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d: %+v", len(items), items)
	}
	if items[0].TaskID != "tmine" {
		t.Errorf("got task_id %q, want tmine", items[0].TaskID)
	}
	if items[0].Action != "review" {
		t.Errorf("got action %q, want review", items[0].Action)
	}
}

// TestListInbox_InlinesUpstreamSubmission pins the bot-content
// inlining: the inbox item carries the upstream task's latest
// submission so the reviewer reads the work without claiming.
//
// We synthesize the claim+submission rows directly rather than
// going through ApplyPlan — avoids the :memory: pool-connection
// flakiness the other emission tests hit, and the goal here is
// the inlining behavior, not the submission machinery.
func TestListInbox_InlinesUpstreamSubmission(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID
	alice := createTestCitizen(t, s, "alice", "tok-up")

	// Upstream task in accepted state with a synthesized claim
	// + submission attached.
	now := time.Now()
	if err := s.CreateTask(&TaskRecord{
		ID: "tup", RunID: runID, Seq: 1, TaskDefID: "tup",
		Action: "answer", ResultType: "text",
		State: TaskAccepted, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.db.Exec(
		`INSERT INTO task_claims (task_id, citizen_id, claimed_at, deadline, outcome, branch, iter_seq)
		 VALUES (?, ?, ?, ?, 'accepted', '', 1)`,
		"tup", alice, now, now.Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("insert claim: %v", err)
	}
	claimID, _ := res.LastInsertId()
	if _, err := s.db.Exec(
		`INSERT INTO task_submissions (claim_id, submitted_at, commit_sha, decision, option, content)
		 VALUES (?, ?, 'abc1234', '', '', 'drafted answer text')`,
		claimID, now,
	); err != nil {
		t.Fatalf("insert submission: %v", err)
	}

	// Reviewer task assigned to tamer, depends on the upstream.
	if err := s.CreateTask(&TaskRecord{
		ID: "trev", RunID: runID, Seq: 99, TaskDefID: "trev",
		Action: "review", ResultType: "text",
		State: TaskReady, Prompt: "review tup", DependsOn: "tup",
		AssignTo:  `["tamer"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	items, err := s.ListInbox(projectID, "tamer")
	if err != nil {
		t.Fatalf("ListInbox: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0]
	if len(it.Upstream) != 1 {
		t.Fatalf("expected 1 upstream submission, got %d: %+v", len(it.Upstream), it.Upstream)
	}
	up := it.Upstream[0]
	if up.TaskID != "tup" {
		t.Errorf("upstream task_id = %q, want tup", up.TaskID)
	}
	if up.Action != "answer" {
		t.Errorf("upstream action = %q, want answer", up.Action)
	}
	if up.Content != "drafted answer text" {
		t.Errorf("upstream content = %q, want 'drafted answer text'", up.Content)
	}
	if up.CommitSHA != "abc1234" {
		t.Errorf("upstream commit_sha = %q, want abc1234", up.CommitSHA)
	}
}

// TestListInbox_TruncatesLargePrompt pins the 2KB cap on the
// Prompt field. Compute scripts and long-form review prompts
// can be 4-8KB; without a cap each inbox call balloons the
// response. Caller follows up with enju_get_task for the full
// text when needed.
func TestListInbox_TruncatesLargePrompt(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID

	bigPrompt := strings.Repeat("a", inboxPromptCap+500)
	if err := s.CreateTask(&TaskRecord{
		ID: "tbig", RunID: runID, Seq: 1, TaskDefID: "tbig",
		Action: "review", ResultType: "text",
		State: TaskReady, Prompt: bigPrompt,
		AssignTo:  `["tamer"]`,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	items, err := s.ListInbox(projectID, "tamer")
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item, got %d", len(items))
	}
	it := items[0]
	if !it.PromptTruncated {
		t.Error("expected PromptTruncated=true for oversize prompt")
	}
	if len(it.Prompt) > inboxPromptCap+len("...(truncated)") {
		t.Errorf("Prompt length = %d, expected ~%d", len(it.Prompt), inboxPromptCap+len("...(truncated)"))
	}
	if !strings.HasSuffix(it.Prompt, "...(truncated)") {
		t.Error("expected '...(truncated)' suffix on large prompt")
	}
}

// TestListInbox_MultiAssignee pins membership across an array:
// a task assigned to ["alice","tamer"] appears in both alice's
// and tamer's inbox.
func TestListInbox_MultiAssignee(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID

	if err := s.CreateTask(&TaskRecord{
		ID: "tboth", RunID: runID, Seq: 1, TaskDefID: "tboth",
		Action: "review", ResultType: "text",
		State: TaskReady, Prompt: "two reviewers",
		AssignTo:  `["alice","tamer"]`,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	for _, who := range []string{"alice", "tamer"} {
		items, err := s.ListInbox(projectID, who)
		if err != nil {
			t.Fatalf("ListInbox(%s): %v", who, err)
		}
		if len(items) != 1 || items[0].TaskID != "tboth" {
			t.Errorf("ListInbox(%s) = %+v, want one item tboth", who, items)
		}
	}

	// Negative: bob is in neither.
	items, _ := s.ListInbox(projectID, "bob")
	if len(items) != 0 {
		t.Errorf("ListInbox(bob) should be empty, got %+v", items)
	}
}

// TestListInbox_EmptyUsername pins the early-return: empty
// username yields no items, no error. Callers must resolve the
// bearer token to a username before calling.
func TestListInbox_EmptyUsername(t *testing.T) {
	s := newTestStore(t)
	items, err := s.ListInbox(1, "")
	if err != nil {
		t.Errorf("expected nil error for empty username, got %v", err)
	}
	if items != nil {
		t.Errorf("expected nil items, got %+v", items)
	}
}
