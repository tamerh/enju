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

// TestUpdateReadyTasks_ReturnsAssignTo verifies that the cascade
// surfaces assign_to in the ReadiedTask list — parsed from the
// JSON-array shape the engine writes (e.g. `["alice"]`). This
// is the data applyUpdateReadyTasks fans out into one task_ready
// event per assignee for the assigned_task_ready notification
// rule.
func TestUpdateReadyTasks_ReturnsAssignTo(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	now := time.Now()
	if err := s.CreateTask(&TaskRecord{
		ID: "tup", RunID: runID, Seq: 1, TaskDefID: "tup",
		Action: "answer", ResultType: "text",
		State: TaskAccepted, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Mirror the production shape: assign_to is JSON-encoded
	// (engine.materialize writes `["alice"]`, store.spawn writes
	// the same). The cascade must parse this back into a slice.
	if err := s.CreateTask(&TaskRecord{
		ID: "trev", RunID: runID, Seq: 2, TaskDefID: "trev",
		Action: "review", ResultType: "text",
		State: TaskPending, DependsOn: "tup",
		AssignTo:  `["tamer"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(&TaskRecord{
		ID: "tnoassign", RunID: runID, Seq: 3, TaskDefID: "tnoassign",
		Action: "answer", ResultType: "text",
		State: TaskPending, DependsOn: "tup",
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	readied, err := s.UpdateReadyTasks(runID)
	if err != nil {
		t.Fatalf("UpdateReadyTasks: %v", err)
	}
	if len(readied) != 2 {
		t.Fatalf("expected 2 readied (trev, tnoassign), got %d: %+v", len(readied), readied)
	}

	byID := map[string]ReadiedTask{}
	for _, rt := range readied {
		byID[rt.TaskID] = rt
	}

	if rt, ok := byID["trev"]; !ok {
		t.Error("trev missing from readied")
	} else {
		if len(rt.Assignees) != 1 || rt.Assignees[0] != "tamer" {
			t.Errorf("trev.Assignees = %#v, want [tamer]", rt.Assignees)
		}
		if rt.Action != "review" {
			t.Errorf("trev.Action = %q, want review", rt.Action)
		}
		if rt.RunID != runID {
			t.Errorf("trev.RunID = %d, want %d", rt.RunID, runID)
		}
	}

	if rt, ok := byID["tnoassign"]; !ok {
		t.Error("tnoassign missing from readied")
	} else if len(rt.Assignees) != 0 {
		t.Errorf("tnoassign.Assignees = %#v, want [] (unassigned task)", rt.Assignees)
	}
}

// TestApplyCreateTask_BornReadyEmitsTaskReady pins the
// run-creation gap: tasks materialized straight into READY (no
// upstream deps — root tasks at run start, dynamic for_each
// instances, spawned tasks with no `depends_on`) emit a
// task_ready event, so the assigned human gets the
// notification. Pre-fix this fired only on the cascade path,
// so a fresh run landed silently on the assignee's plate.
func TestApplyCreateTask_BornReadyEmitsTaskReady(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	// Insert a task born in READY via ApplyPlan{CreateTask{...}}
	// — this is what materialize.go does for tasks with no deps.
	now := time.Now()
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		CreateTask{Task: TaskRecord{
			ID: "5:1:write_blurb", RunID: runID, Seq: 1, TaskDefID: "write_blurb",
			Action: "answer", ResultType: "text",
			State: TaskReady, AssignTo: `["tamer"]`,
			CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	waitForEventsDrained(t, s)

	got := hasEventWithMetadata(t, s, runID, "task_ready", `"assign_to":"tamer"`)
	if got == nil {
		t.Fatal("expected task_ready event for born-ready task with assign_to=tamer")
	}
	if got.TaskID != "5:1:write_blurb" {
		t.Errorf("task_id = %q, want 5:1:write_blurb", got.TaskID)
	}
	if got.Subtype != "answer" {
		t.Errorf("subtype = %q, want answer (action)", got.Subtype)
	}
}

// TestApplyCreateTask_BornPendingDoesNotEmit pins the negative:
// tasks born in PENDING (waiting on upstream) must NOT fire a
// task_ready event — they fire later via the cascade when their
// deps land. Without this guard, every for_each instance would
// double-emit (once at materialize, once at promote).
func TestApplyCreateTask_BornPendingDoesNotEmit(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	now := time.Now()
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		CreateTask{Task: TaskRecord{
			ID: "5:1:later", RunID: runID, Seq: 1, TaskDefID: "later",
			Action: "answer", ResultType: "text",
			State: TaskPending, DependsOn: "5:1:something",
			AssignTo:  `["tamer"]`,
			CreatedAt: now,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	waitForEventsDrained(t, s)

	if hasEventWithMetadata(t, s, runID, "task_ready", `"task_id":"5:1:later"`) != nil {
		t.Error("PENDING task must not emit task_ready at create time")
	}
}

// TestApplySetTaskState_ReboundReadyEmitsTaskReady pins the
// request_changes gap: when the cascade flips a target task
// ACCEPTED→READY via SetTaskState{ClearClaim:true,
// NewState:TaskReady}, the assignee gets a task_ready event so
// they know their work needs revision. Pre-fix this fired
// nothing — only task_request_changes + cascade_fired emitted.
func TestApplySetTaskState_ReboundReadyEmitsTaskReady(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	// Set up a task in ACCEPTED state — the cascade target.
	now := time.Now()
	if err := s.CreateTask(&TaskRecord{
		ID: "5:1:write_blurb", RunID: runID, Seq: 1, TaskDefID: "write_blurb",
		Action: "answer", ResultType: "text",
		State: TaskAccepted, AssignTo: `["tamer"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Simulate the request_changes cascade rebounding the
	// target back to READY with claim cleared.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{
			TaskID:     "5:1:write_blurb",
			NewState:   TaskReady,
			ClearClaim: true,
		},
	}}); err != nil {
		t.Fatal(err)
	}
	waitForEventsDrained(t, s)

	got := hasEventWithMetadata(t, s, runID, "task_ready", `"assign_to":"tamer"`)
	if got == nil {
		t.Fatal("expected task_ready event after request_changes rebound to READY")
	}
	if got.TaskID != "5:1:write_blurb" {
		t.Errorf("task_id = %q, want 5:1:write_blurb", got.TaskID)
	}
}

// TestUpdateReadyTasks_DirectCallEmitsEvents pins the
// production-load-bearing path: most callers invoke
// s.UpdateReadyTasks(runID) directly (after ApplyPlan returns),
// not as a Plan mutation. That direct path must emit task_ready
// events too, otherwise the assigned_task_ready notification
// rule never fires in the standard submit→cascade flow. Pre-fix
// the emit lived only in applyUpdateReadyTasks (the mutation
// handler), so direct callers — vote/review submit, invalidate,
// fail-cascade — silently dropped task_ready events.
func TestUpdateReadyTasks_DirectCallEmitsEvents(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	now := time.Now()
	if err := s.CreateTask(&TaskRecord{
		ID: "tup", RunID: runID, Seq: 1, TaskDefID: "tup",
		Action: "answer", ResultType: "text",
		State: TaskAccepted, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.CreateTask(&TaskRecord{
		ID: "trev", RunID: runID, Seq: 2, TaskDefID: "trev",
		Action: "review", ResultType: "text",
		State: TaskPending, DependsOn: "tup",
		AssignTo:  `["alice"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	readied, err := s.UpdateReadyTasks(runID)
	if err != nil {
		t.Fatalf("UpdateReadyTasks: %v", err)
	}
	if len(readied) != 1 {
		t.Fatalf("expected 1 readied, got %d", len(readied))
	}

	waitForEventsDrained(t, s)
	if hasEventWithMetadata(t, s, runID, "task_ready", `"assign_to":"alice"`) == nil {
		t.Fatal("direct s.UpdateReadyTasks call must emit task_ready event with assign_to in metadata")
	}
}

// TestApplyPlan_CascadeSeesInTxWrites pins the tx-aware
// behavior: a Plan that flips an upstream task to ACCEPTED
// and runs the cascade in the same Plan must see the new
// state and promote downstream tasks. Pre-fix the cascade ran
// via s.db (separate connection, pre-commit view), so it
// silently saw the upstream as still claimed/collecting and
// promoted nothing — the deadline-driven vote/review resolve
// path's bug.
func TestApplyPlan_CascadeSeesInTxWrites(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-tx")

	// Drive an upstream task through claim+submit so it lands
	// in collecting/accepted state via the standard path.
	taskID := makeTaskWithAction(t, s, runID, "tup", "answer", TaskReady)
	deadline := time.Now().Add(time.Hour)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: taskID, CitizenID: alice, Deadline: deadline},
	}}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		RecordSubmission{
			TaskID: taskID, CitizenID: alice,
			CommitSHA: "deadbeef", Content: "x", TokensUsed: 1, EstimatedTokens: 1,
		},
	}}); err != nil {
		t.Fatal(err)
	}

	// Insert a downstream task that depends on the upstream.
	now := time.Now()
	if err := s.CreateTask(&TaskRecord{
		ID: "tdown", RunID: runID, Seq: 99, TaskDefID: "tdown",
		Action: "review", ResultType: "text",
		State: TaskPending, DependsOn: taskID,
		AssignTo:  `["alice"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Single Plan: bump tup to ACCEPTED + run cascade. The
	// cascade must see the in-tx accept and promote tdown.
	result, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: taskID, NewState: TaskAccepted, CommitSHA: "deadbeef"},
		UpdateReadyTasks{RunID: runID},
	}})
	if err != nil {
		t.Fatalf("ApplyPlan: %v", err)
	}

	if result.TasksReadied != 1 {
		t.Errorf("TasksReadied = %d, want 1 — cascade did not see the in-tx accept", result.TasksReadied)
	}

	// Verify post-commit: tdown is now ready.
	tdown, _ := s.GetTask("tdown")
	if tdown == nil {
		t.Fatal("tdown missing")
	}
	if tdown.State != TaskReady {
		t.Errorf("tdown.State = %q, want ready", tdown.State)
	}

	// Verify the task_ready event landed for alice.
	waitForEventsDrained(t, s)
	if hasEventWithMetadata(t, s, runID, "task_ready", `"assign_to":"alice"`) == nil {
		t.Error("missing task_ready event for alice")
	}
}

// TestUpdateReadyTasks_ParsesJSONArrayAssignTo pins the parse
// of the on-disk shape: tasks.assign_to is a JSON-encoded array
// (`["alice"]`) per engine.materialize / store.spawn. The
// cascade must unmarshal it before handing off to the apply
// emit-site — without this, the wire-level assign_to leaks the
// JSON literal and predicate matchers never fire.
func TestUpdateReadyTasks_ParsesJSONArrayAssignTo(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)

	now := time.Now()
	if err := s.CreateTask(&TaskRecord{
		ID: "tup", RunID: runID, Seq: 1, TaskDefID: "tup",
		Action: "answer", ResultType: "text",
		State: TaskAccepted, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Single-assignee, JSON-array shape.
	if err := s.CreateTask(&TaskRecord{
		ID: "tsingle", RunID: runID, Seq: 2, TaskDefID: "tsingle",
		Action: "review", ResultType: "text",
		State: TaskPending, DependsOn: "tup",
		AssignTo:  `["alice"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	// Multi-assignee, JSON-array shape.
	if err := s.CreateTask(&TaskRecord{
		ID: "tdual", RunID: runID, Seq: 3, TaskDefID: "tdual",
		Action: "review", ResultType: "text",
		State: TaskPending, DependsOn: "tup",
		AssignTo:  `["alice","bob"]`,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	readied, err := s.UpdateReadyTasks(runID)
	if err != nil {
		t.Fatalf("UpdateReadyTasks: %v", err)
	}
	byID := map[string]ReadiedTask{}
	for _, rt := range readied {
		byID[rt.TaskID] = rt
	}

	if rt := byID["tsingle"]; len(rt.Assignees) != 1 || rt.Assignees[0] != "alice" {
		t.Errorf("tsingle.Assignees = %#v, want [alice] (parsed from JSON-array)", rt.Assignees)
	}
	if rt := byID["tdual"]; len(rt.Assignees) != 2 || rt.Assignees[0] != "alice" || rt.Assignees[1] != "bob" {
		t.Errorf("tdual.Assignees = %#v, want [alice bob]", rt.Assignees)
	}
}

// TestBuildTaskReadyEvents pins the fan-out shape that
// applyUpdateReadyTasks delegates to. Pure function — easier to
// test than the ApplyPlan-driven path (which has its own
// pre-existing same-process write-lock interaction with
// s.UpdateReadyTasks). Covers:
//
//   - single-assignee → one event with bare username in metadata
//   - multi-assignee → N events, one per assignee
//   - unassigned → one event with empty assign_to (audit row)
func TestBuildTaskReadyEvents(t *testing.T) {
	now := time.Date(2026, 5, 2, 12, 0, 0, 0, time.UTC)
	readied := []ReadiedTask{
		{TaskID: "trev", Action: "review", Assignees: []string{"alice"}, RunID: 7, ProjectID: 1},
		{TaskID: "tdual", Action: "review", Assignees: []string{"alice", "bob"}, RunID: 7, ProjectID: 1},
		{TaskID: "tnoassign", Action: "answer", Assignees: nil, RunID: 7, ProjectID: 1},
	}

	events := buildTaskReadyEvents(readied, now)

	// trev → 1, tdual → 2, tnoassign → 1: total 4.
	if len(events) != 4 {
		t.Fatalf("expected 4 events, got %d", len(events))
	}

	// Every event should be type=task_ready with the right subtype.
	// Pull assign_to from metadata to verify the fan-out and the
	// bare-username shape (no JSON-array literal).
	type emitted struct {
		taskID, subtype, assignTo string
	}
	var got []emitted
	for _, e := range events {
		if e.EventType != "task_ready" {
			t.Errorf("event type = %q, want task_ready", e.EventType)
		}
		if e.RunID != 7 || e.ProjectID != 1 {
			t.Errorf("event scope = run=%d project=%d, want run=7 project=1", e.RunID, e.ProjectID)
		}
		// The metadata is a marshaled string; use contains to
		// avoid a JSON parse — tests stay readable.
		var assignTo string
		if contains(e.Metadata, `"assign_to":"alice"`) {
			assignTo = "alice"
		} else if contains(e.Metadata, `"assign_to":"bob"`) {
			assignTo = "bob"
		} else if contains(e.Metadata, `"assign_to":""`) {
			assignTo = ""
		} else {
			t.Errorf("unexpected metadata: %s", e.Metadata)
		}
		got = append(got, emitted{e.TaskID, e.EventSubtype, assignTo})
	}

	want := []emitted{
		{"trev", "review", "alice"},
		{"tdual", "review", "alice"},
		{"tdual", "review", "bob"},
		{"tnoassign", "answer", ""},
	}
	for i, w := range want {
		if got[i] != w {
			t.Errorf("event[%d] = %+v, want %+v", i, got[i], w)
		}
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
