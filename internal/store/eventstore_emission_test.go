package store

// /7g — regression-pin tests for the new event types.
// These pair with eventstore_contract_test.go: the contract
// tests guard the strict-consumer architecture (events never
// on critical path); these tests guard the emission contract
// (every load-bearing state mutation produces the expected
// audit row). A future refactor that drops one of these
// emissions silently would otherwise break the audit timeline
// without anything else failing.

import (
	"testing"
	"time"
)

// hasEventWithMetadata returns the first event of the given
// type for the run whose metadata JSON contains `needle`. nil
// if none. Tests use this to assert "an event of type X with
// the right contextual key landed" without writing per-event
// JSON parsers.
func hasEventWithMetadata(t *testing.T, s *Store, runID int64, eventType, needle string) *RunEventRecord {
	t.Helper()
	waitForEventsDrained(t, s)
	events, err := s.ListEvents(EventQuery{
		RunID:   runID,
		EventTypes: []string{eventType},
		Limit:   100,
	})
	if err != nil {
		t.Fatalf("ListEvents(%s): %v", eventType, err)
	}
	for i, e := range events {
		if needle == "" || (e.Metadata != "" && contains(e.Metadata, needle)) {
			return &events[i]
		}
	}
	return nil
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// claimAndSubmit is the shared scaffold: insert a single-citizen
// task in READY, claim it via ApplyPlan, then submit. Returns
// the claim's iteration branch so tests can assert
// iteration_started's metadata. Used by the unreviewed-completed
// and submission-attempt tests.
func claimAndSubmit(t *testing.T, s *Store, runID int64, citizenID int64,
	taskDefID, action string) (taskID, branch string) {
	t.Helper()
	taskID = makeTaskWithAction(t, s, runID, taskDefID, action, TaskReady)
	deadline := time.Now().Add(time.Hour)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: citizenID, Deadline: deadline},
	}}); err != nil {
		t.Fatalf("claim: %v", err)
	}
	claims, _ := s.ListActiveClaims(taskID)
	if len(claims) > 0 {
		branch = claims[0].Branch
	}
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		RecordSubmission{
			TaskID:     taskID,
			CitizenID:    citizenID,
			CommitSHA:    "deadbeef" + taskDefID,
			Content:     "result",
			TokensUsed:   10,
			EstimatedTokens: 25,
		},
	}}); err != nil {
		t.Fatalf("submit: %v", err)
	}
	return taskID, branch
}

// makeTaskWithAction is the test-helper analogue of makeTask
// but lets us pick the action (so a review task can be inserted
// alongside its target). Inserts directly via the engine row
// shape to bypass the parser; sufficient for store-level tests.
func makeTaskWithAction(t *testing.T, s *Store, runID int64, defID, action string, state TaskState) string {
	t.Helper()
	now := time.Now()
	// Read run for project/seq context.
	r, err := s.GetRun(runID)
	if err != nil || r == nil {
		t.Fatalf("GetRun: %v", err)
	}
	taskID := defID // tests use def_id directly as the task id; mirrors makeTask.
	_, err = s.db.Exec(
		`INSERT INTO tasks (id, run_id, seq, task_def_id, instance_key, instance_params, ref,
			action, prompt, user_prompt, script, outputs, requirements, result_type,
			timeout, state, depends_on, reads_artifacts, writes_artifacts,
			assign_to, require_role, citizens, run_slug,
			spawned_from, spawn_trigger, closes_issue_seq, created_at)
		 VALUES (?, ?, 1, ?, '', '', '',
			?, '', '', '', '', '', 'text',
			'', ?, '', '[]', '[]',
			'', '', 1, '',
			'', '', 0, ?)`,
		taskID, runID, defID, action, state, now,
	)
	if err != nil {
		t.Fatalf("insert task: %v", err)
	}
	_ = r
	return taskID
}

// ---- Tests ----

// TestEventEmission_IterationStartedFires verifies the most
// load-bearing emission: every claim creates an
// iteration_started event with iter_seq=1 + the iteration
// branch in metadata. .1.
func TestEventEmission_IterationStartedFires(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-it1")
	taskID, branch := claimAndSubmit(t, s, runID, alice, "1:1:t", "answer")
	_ = taskID
	if branch == "" {
		t.Fatal("expected an iteration branch on a single-citizen answer claim")
	}
	got := hasEventWithMetadata(t, s, runID, "iteration_started", `"iter_seq":1`)
	if got == nil {
		t.Fatalf("no iteration_started event with iter_seq=1 found")
	}
	if got.Subtype != "fresh" {
		t.Errorf("subtype = %q, want fresh", got.Subtype)
	}
	if !contains(got.Metadata, branch) {
		t.Errorf("metadata %q missing iteration_branch %q", got.Metadata, branch)
	}
}

// TestEventEmission_TaskSubmittedFires verifies every submission
// (including stay-open reviewed paths) produces a task_submitted
// event with iter_seq + commit_sha + attempt_seq. .3.
func TestEventEmission_TaskSubmittedFires(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-sub")
	claimAndSubmit(t, s, runID, alice, "1:1:t", "answer")
	got := hasEventWithMetadata(t, s, runID, "task_submitted", `"attempt_seq":1`)
	if got == nil {
		t.Fatalf("no task_submitted event with attempt_seq=1 found")
	}
	if got.Subtype != "answer" {
		t.Errorf("subtype = %q, want answer", got.Subtype)
	}
	if !contains(got.Metadata, "deadbeef") {
		t.Errorf("metadata %q missing commit_sha", got.Metadata)
	}
}

// TestEventEmission_TaskCompletedAndIterationCompletedUnreviewed
// verifies an unreviewed single-citizen submit fires both
// task_completed and iteration_completed("completed") at the
// same submit moment.
func TestEventEmission_TaskCompletedAndIterationCompletedUnreviewed(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-c1")
	claimAndSubmit(t, s, runID, alice, "1:1:t", "answer")

	tc := hasEventWithMetadata(t, s, runID, "task_completed", `"reviewed":false`)
	if tc == nil {
		t.Fatal("no task_completed event with reviewed=false found")
	}
	ic := hasEventWithMetadata(t, s, runID, "iteration_completed", "")
	if ic == nil || ic.Subtype != "completed" {
		t.Fatalf("expected iteration_completed(completed), got %+v", ic)
	}
}

// TestEventEmission_TaskCompletedFiresOnReviewApprove is the
// regression test for the review-approve emission path. A
// reviewed task's claim stays open until the
// reviewer approves; MarkLatestClaimOutcome("completed") MUST
// emit task_completed AND iteration_completed for the upstream
// even though applySetTaskState never fires.
func TestEventEmission_TaskCompletedFiresOnReviewApprove(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-rva")

	// Set up the upstream task and simulate it being in the
	// "submitted but stay-open" state: state=accepted on the
	// task row (single-citizen unreviewed paths set this), but
	// the claim row is still open (outcome=NULL) with a
	// denormalized commit_sha. This mirrors the real shape
	// after a reviewed answer task submits but before the
	// review approves.
	taskID := makeTaskWithAction(t, s, runID, "1:1:upstream", "answer", TaskReady)
	deadline := time.Now().Add(time.Hour)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: deadline},
	}}); err != nil {
		t.Fatal(err)
	}
	// Manually mark the task accepted + denormalize commit_sha
	// onto the open claim row, mimicking applyRecordSubmission's
	// stayOpen branch.
	if _, err := s.db.Exec(
		`UPDATE tasks SET state = 'accepted', commit_sha = ? WHERE id = ?`,
		"feedface", taskID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`UPDATE task_claims SET commit_sha = ? WHERE task_id = ? AND outcome IS NULL`,
		"feedface", taskID,
	); err != nil {
		t.Fatal(err)
	}

	// Drain pre-existing events so the assertions below can't
	// match an earlier iteration_started.
	waitForEventsDrained(t, s)

	// Now trigger the review-approve path.
	if _, err := s.MarkLatestClaimOutcome(taskID, "completed"); err != nil {
		t.Fatalf("MarkLatestClaimOutcome: %v", err)
	}

	tc := hasEventWithMetadata(t, s, runID, "task_completed", `"reviewed":true`)
	if tc == nil {
		t.Fatal("review-approve path did not emit task_completed (issue 1)")
	}
	if !contains(tc.Metadata, "feedface") {
		t.Errorf("task_completed metadata %q missing commit_sha=feedface", tc.Metadata)
	}
	ic := hasEventWithMetadata(t, s, runID, "iteration_completed", "")
	if ic == nil || ic.Subtype != "completed" {
		t.Fatalf("review-approve path did not emit iteration_completed(completed), got %+v", ic)
	}
}

// TestEventEmission_IterationStartedReopen verifies a takeover
// by a different citizen produces iteration_started("reopen")
// + iteration_completed("abandoned") for the prior claim.
func TestEventEmission_IterationStartedReopen(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-rop-a")
	bob := createTestCitizen(t, s, "bob", "tok-rop-b")

	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "answer", TaskReady)
	deadline := time.Now().Add(time.Hour)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: deadline},
	}}); err != nil {
		t.Fatal(err)
	}
	// Bob takes over — same task, different citizen → abandon
	// alice's claim, start a fresh one with iter_seq=2.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: bob, Deadline: deadline},
	}}); err != nil {
		t.Fatal(err)
	}
	ab := hasEventWithMetadata(t, s, runID, "iteration_completed", "taken_over_by")
	if ab == nil || ab.Subtype != "abandoned" {
		t.Fatalf("takeover did not emit iteration_completed(abandoned), got %+v", ab)
	}
	reopen := hasEventWithMetadata(t, s, runID, "iteration_started", `"iter_seq":2`)
	if reopen == nil {
		t.Fatal("takeover did not emit iteration_started for the new claim")
	}
	if reopen.Subtype != "reopen" {
		t.Errorf("subtype = %q, want reopen", reopen.Subtype)
	}
}

// TestEventEmission_IterationCompletedOnInvalidate verifies
// MarkOpenClaimsInvalidated emits iteration_completed(invalidated)
// for every closed claim. .2.
func TestEventEmission_IterationCompletedOnInvalidate(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-inv")

	taskID := makeTaskWithAction(t, s, runID, "1:1:t", "answer", TaskReady)
	deadline := time.Now().Add(time.Hour)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: deadline},
	}}); err != nil {
		t.Fatal(err)
	}
	waitForEventsDrained(t, s)

	if _, err := s.MarkOpenClaimsInvalidated(taskID); err != nil {
		t.Fatalf("MarkOpenClaimsInvalidated: %v", err)
	}
	ic := hasEventWithMetadata(t, s, runID, "iteration_completed", "cascade_invalidate")
	if ic == nil {
		t.Fatal("MarkOpenClaimsInvalidated did not emit iteration_completed(invalidated)")
	}
	if ic.Subtype != "invalidated" {
		t.Errorf("subtype = %q, want invalidated", ic.Subtype)
	}
}
