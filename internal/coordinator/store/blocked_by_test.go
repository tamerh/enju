package store

// Phase 8.5 blocked_by tests. Pin one setup-and-assert test per
// kind so the priority order (review > human_claim > artifact >
// stuck) and the fields each kind populates can't drift without
// loud failure. These run against the production transition site
// — applyCompleteRun fires computeBlockedBy when the run-state
// evaluator lands on RunWaiting; we drive that by inserting
// tasks in the relevant in-flight states and applying CompleteRun.

import (
	"testing"
	"time"
)

// runRunStateEvaluator applies CompleteRun via the same
// chokepoint production uses. The mutation re-reads task
// counts and lands the run on the right state — for these
// tests, we set up tasks that force RunWaiting so blocked_by
// gets populated.
func runRunStateEvaluator(t *testing.T, s *Store, runID int64) {
	t.Helper()
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		CompleteRun{RunID: runID},
	}}); err != nil {
		t.Fatalf("CompleteRun: %v", err)
	}
}

// callComputeBlockedBy is a tx-local invocation of the helper
// for tests whose target scenarios can't reach WAITING via
// the run-state evaluator's existing rule (CLAIMED/READY tasks
// keep the run ACTIVE). Direct invocation pins the priority
// logic independent of how WAITING was entered.
//
// Phase 8.5 design note: review and human_claim kinds presume
// future evaluator extensions (stale-claim detection, all-
// READY-only-human-assigned WAITING) that aren't part of this
// commit's scope. The kinds compile correctly and surface
// correctly through the column when reached via auto-triage's
// open-issues override; this helper exercises them directly
// so the priority + field shape stays pinned regardless of
// how WAITING is reached.
func callComputeBlockedBy(t *testing.T, s *Store, runID int64) *BlockedBy {
	t.Helper()
	tx, err := s.db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback()
	got, err := computeBlockedBy(tx, runID)
	if err != nil {
		t.Fatalf("computeBlockedBy: %v", err)
	}
	return ParseBlockedBy(got)
}

// TestBlockedBy_ReviewKindWinsOverHumanClaim pins the priority
// rule: when both a CLAIMED review and a READY human-assigned
// non-review task exist, the review wins. Reviewer-bottleneck
// is the most actionable signal for the operator.
func TestBlockedBy_ReviewKindWinsOverHumanClaim(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-rev")
	now := time.Now()

	// A review task in CLAIMED — alice is sitting on it.
	// applyCreateTask doesn't bind claimed_by/claimed_at
	// (those land via SetClaim mutations in production); set
	// them directly so the test pins exact field values.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:gate", RunID: runID, Seq: 10, TaskDefID: "gate",
		Action: "review", ResultType: "text", State: TaskClaimed,
		ReviewsTarget: "draft", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`UPDATE tasks SET claimed_by = ?, claimed_at = ? WHERE id = ?`,
		alice, now.Add(-2*time.Hour), "1:1:gate",
	); err != nil {
		t.Fatal(err)
	}
	// A READY human-assigned non-review task. Would be the
	// human_claim blocker if review weren't present.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:edit", RunID: runID, Seq: 20, TaskDefID: "edit",
		Action: "answer", ResultType: "text", State: TaskReady,
		AssignTo: `["bob"]`, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	bb := callComputeBlockedBy(t, s, runID)
	if bb == nil {
		t.Fatal("blocked_by missing")
	}
	if bb.Kind != BlockedByReview {
		t.Errorf("Kind = %q, want review (priority rule: review wins over human_claim)", bb.Kind)
	}
	if bb.Task != "1:1:gate" {
		t.Errorf("Task = %q, want 1:1:gate", bb.Task)
	}
	if bb.Assignee != "alice" {
		t.Errorf("Assignee = %q, want alice", bb.Assignee)
	}
	if bb.Since == "" {
		t.Errorf("Since must be populated for review kind, got empty")
	}
}

// TestBlockedBy_HumanClaimKind pins the second-priority case:
// no in-flight review, but a non-review READY task is assigned
// to a human and unclaimed. First-listed assignee surfaces.
func TestBlockedBy_HumanClaimKind(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	now := time.Now()

	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:write", RunID: runID, Seq: 1, TaskDefID: "write",
		Action: "answer", ResultType: "text", State: TaskReady,
		AssignTo: `["alice","bob"]`, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	bb := callComputeBlockedBy(t, s, runID)
	if bb == nil {
		t.Fatal("blocked_by missing")
	}
	if bb.Kind != BlockedByHumanClaim {
		t.Errorf("Kind = %q, want human_claim", bb.Kind)
	}
	if bb.Task != "1:1:write" {
		t.Errorf("Task = %q, want 1:1:write", bb.Task)
	}
	if bb.Assignee != "alice" {
		t.Errorf("Assignee = %q, want alice (first assignee)", bb.Assignee)
	}
}

// TestBlockedBy_ArtifactKind pins the third-priority case: a
// PENDING task's reads_artifacts can't be satisfied because
// every candidate writer is in a non-terminal state. Phase
// 8.3's deferred-accept gate is what makes this a real
// scenario — the artifact row exists at submit time but the
// writer is SUBMITTED until /merges fires.
func TestBlockedBy_ArtifactKind(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID
	alice := createTestCitizen(t, s, "alice", "tok-art")
	now := time.Now()

	// Writer task in SUBMITTED — artifact-state gate hides
	// its row from cascade readers.
	writerID := makeTaskWithAction(t, s, runID, "writer", "answer", TaskReady)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: writerID, CitizenID: alice, Deadline: time.Now().Add(time.Hour)},
		RecordSubmission{TaskID: writerID, CitizenID: alice, CommitSHA: "deadbeef", TokensUsed: 1, EstimatedTokens: 1},
		MoveArtifact{Artifact: ArtifactRecord{
			ProjectID: projectID, Branch: "main", Path: "out/data.json",
			LastTaskID: writerID, LastWriter: alice, LastRunID: runID,
			CommitSHA: "deadbeef", Tracked: true,
			CreatedAt: now, UpdatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("setup writer: %v", err)
	}

	// Pending reader gated on the artifact.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:reader", RunID: runID, Seq: 99, TaskDefID: "reader",
		Action: "answer", ResultType: "text", State: TaskPending,
		ReadsArtifacts: `["out/data.json"]`,
		CreatedAt:      now,
	}); err != nil {
		t.Fatal(err)
	}

	bb := callComputeBlockedBy(t, s, runID)
	if bb == nil {
		t.Fatal("blocked_by missing")
	}
	if bb.Kind != BlockedByArtifact {
		t.Errorf("Kind = %q, want artifact", bb.Kind)
	}
	if bb.Task != "1:1:reader" {
		t.Errorf("Task = %q, want 1:1:reader", bb.Task)
	}
	if bb.AwaitingPath != "out/data.json" {
		t.Errorf("AwaitingPath = %q, want out/data.json", bb.AwaitingPath)
	}
}

// TestBlockedBy_StuckFallback pins the fourth-priority case:
// none of the higher-priority rules match. Indicates a system
// bug or a state the evaluator doesn't classify yet — surface
// the fact so the operator can investigate rather than seeing
// silent NULL.
func TestBlockedBy_StuckFallback(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	now := time.Now()

	// A pending task with no reads_artifacts and no review/
	// human-assignee. Won't match any specific blocker.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:lonely", RunID: runID, Seq: 1, TaskDefID: "lonely",
		Action: "answer", ResultType: "text", State: TaskPending,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	runRunStateEvaluator(t, s, runID)
	run, _ := s.GetRun(runID)
	if run.State != RunWaiting {
		t.Fatalf("run state = %q, want waiting", run.State)
	}
	bb := ParseBlockedBy(run.BlockedBy)
	if bb == nil {
		t.Fatalf("blocked_by missing or unparseable: %q", run.BlockedBy)
	}
	if bb.Kind != BlockedByStuck {
		t.Errorf("Kind = %q, want stuck (no other rule matched)", bb.Kind)
	}
	if bb.Detail == "" {
		t.Errorf("Detail must be non-empty for stuck kind, got empty")
	}
}

// TestBlockedBy_PriorityChain pins the full priority order
// (review > human_claim > artifact > stuck) by setting up a
// run that satisfies all four conditions simultaneously and
// progressively stripping the higher-priority blocker. After
// each strip the next-priority kind must surface — proves the
// switchboard is monotonic and no kind silently masks another.
//
// Setup mirrors a real review-pending workflow with a stale
// downstream side-task and a deferred-accept artifact gate:
//   - gate: review CLAIMED by alice (review kind candidate)
//   - edit: answer READY assigned to bob (human_claim
//     candidate)
//   - reader: answer PENDING reading an artifact whose writer
//     is SUBMITTED (artifact candidate)
//   - lonely: answer PENDING with no other signal (forces
//     stuck fallback at the end of the strip chain)
func TestBlockedBy_PriorityChain(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID
	alice := createTestCitizen(t, s, "alice", "tok-pri-a")
	now := time.Now()

	// review-kind candidate: gate task CLAIMED by alice.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:gate", RunID: runID, Seq: 10, TaskDefID: "gate",
		Action: "review", ResultType: "text", State: TaskClaimed,
		ReviewsTarget: "draft", CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`UPDATE tasks SET claimed_by = ?, claimed_at = ? WHERE id = ?`,
		alice, now.Add(-2*time.Hour), "1:1:gate",
	); err != nil {
		t.Fatal(err)
	}

	// human_claim-kind candidate: edit task READY assigned to bob.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:edit", RunID: runID, Seq: 20, TaskDefID: "edit",
		Action: "answer", ResultType: "text", State: TaskReady,
		AssignTo: `["bob"]`, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// artifact-kind candidate: writer in SUBMITTED with an
	// artifact-index row pointing at it; reader PENDING on
	// that path. Mirrors Phase 8.3 deferred-accept gate
	// shape.
	writerID := makeTaskWithAction(t, s, runID, "writer", "answer", TaskReady)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: writerID, CitizenID: alice, Deadline: time.Now().Add(time.Hour)},
		RecordSubmission{TaskID: writerID, CitizenID: alice, CommitSHA: "deadbeef", TokensUsed: 1, EstimatedTokens: 1},
		MoveArtifact{Artifact: ArtifactRecord{
			ProjectID: projectID, Branch: "main", Path: "out/data.json",
			LastTaskID: writerID, LastWriter: alice, LastRunID: runID,
			CommitSHA: "deadbeef", Tracked: true,
			CreatedAt: now, UpdatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("setup writer/artifact: %v", err)
	}
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:reader", RunID: runID, Seq: 99, TaskDefID: "reader",
		Action: "answer", ResultType: "text", State: TaskPending,
		ReadsArtifacts: `["out/data.json"]`, CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// stuck-fallback candidate: a pending task with no
	// signals. Won't match any specific rule once the
	// higher-priority candidates are stripped.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:lonely", RunID: runID, Seq: 999, TaskDefID: "lonely",
		Action: "answer", ResultType: "text", State: TaskPending,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	// Step 1 — all four candidates present. review wins.
	bb := callComputeBlockedBy(t, s, runID)
	if bb == nil || bb.Kind != BlockedByReview {
		t.Fatalf("step 1: expected review, got %+v", bb)
	}

	// Step 2 — strip the review (release alice's claim) and
	// assert human_claim wins.
	if _, err := s.db.Exec(
		`UPDATE tasks SET state = 'ready', claimed_by = NULL, claimed_at = NULL WHERE id = ?`,
		"1:1:gate",
	); err != nil {
		t.Fatal(err)
	}
	bb = callComputeBlockedBy(t, s, runID)
	if bb == nil || bb.Kind != BlockedByHumanClaim {
		t.Fatalf("step 2: expected human_claim, got %+v", bb)
	}
	if bb.Task != "1:1:edit" {
		t.Errorf("step 2: human_claim should pick edit (lowest seq among READY+assigned), got %q", bb.Task)
	}

	// Step 3 — strip the human_claim candidate (clear
	// assign_to on the gate row too, since gate is now
	// READY without an assignee — a tie. edit is the only
	// human-assigned READY task, so clearing it removes the
	// kind). Drop edit's assign_to and gate's review-action
	// so neither shows as a human-assigned ready task.
	if _, err := s.db.Exec(`UPDATE tasks SET assign_to = '' WHERE id = ?`, "1:1:edit"); err != nil {
		t.Fatal(err)
	}
	bb = callComputeBlockedBy(t, s, runID)
	if bb == nil || bb.Kind != BlockedByArtifact {
		t.Fatalf("step 3: expected artifact, got %+v", bb)
	}
	if bb.AwaitingPath != "out/data.json" {
		t.Errorf("step 3: artifact path = %q, want out/data.json", bb.AwaitingPath)
	}

	// Step 4 — strip the artifact candidate (drop the
	// reader's reads_artifacts) and assert stuck wins.
	if _, err := s.db.Exec(
		`UPDATE tasks SET reads_artifacts = '[]' WHERE id = ?`, "1:1:reader",
	); err != nil {
		t.Fatal(err)
	}
	bb = callComputeBlockedBy(t, s, runID)
	if bb == nil || bb.Kind != BlockedByStuck {
		t.Fatalf("step 4: expected stuck, got %+v", bb)
	}
	if bb.Detail == "" {
		t.Errorf("step 4: stuck Detail must be non-empty, got empty")
	}
}

// TestBlockedBy_ClearsOnLeavingWaiting pins the lifecycle
// invariant: a run that transitions WAITING → ACTIVE has its
// blocked_by column cleared. A surface reader who sees
// state=active must NEVER see a stale blocker from a prior
// WAITING window.
func TestBlockedBy_ClearsOnLeavingWaiting(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	now := time.Now()

	// Land in WAITING via a pending-only task graph.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:hold", RunID: runID, Seq: 1, TaskDefID: "hold",
		Action: "answer", ResultType: "text", State: TaskPending,
		CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	runRunStateEvaluator(t, s, runID)
	run, _ := s.GetRun(runID)
	if run.State != RunWaiting {
		t.Fatalf("setup wrong: expected waiting, got %q", run.State)
	}
	if run.BlockedBy == "" {
		t.Fatalf("expected blocked_by set on waiting run, got empty")
	}

	// Flip task to READY → run should land on ACTIVE → blocker clears.
	if _, err := s.db.Exec(`UPDATE tasks SET state = 'ready' WHERE id = ?`, "1:1:hold"); err != nil {
		t.Fatal(err)
	}
	runRunStateEvaluator(t, s, runID)
	run, _ = s.GetRun(runID)
	if run.State != RunActive {
		t.Fatalf("expected active after flip, got %q", run.State)
	}
	if run.BlockedBy != "" {
		t.Errorf("blocked_by must clear on leaving waiting, got %q", run.BlockedBy)
	}
}
