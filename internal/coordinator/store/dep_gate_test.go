package store

// Phase 8.3 dual-tier dep-gate tests. The cascade's
// applyUpdateReadyTasks loop applies two different gates to a
// pending task's deps, and the dual-tier semantic is what makes
// review-pattern workflows function under the new SUBMITTED
// state:
//
//   1. depends_on (task-to-task structural dep): satisfied when
//      the upstream is in {accepted, submitted, skipped}. The
//      SUBMITTED inclusion is what lets reviewer tasks claim
//      their target during the merge-pending window — without
//      it, a single-citizen reviewed task would land in
//      SUBMITTED and the reviewer would stay in PENDING
//      forever (no /merges call could fire because no review
//      has happened yet).
//
//   2. reads_artifacts (task-to-content dep): satisfied only
//      when the writer task is in {accepted, skipped}. Stays
//      stricter because content-reading downstream needs the
//      merge to confirm — the artifact-index row exists at
//      submit time but its underlying commit is on the topic
//      branch, not yet on the run branch. Fanning a content
//      consumer out before the merge is the silent-cascade-stall
//      bug Phase 8.3 closes.
//
// These tests pin the asymmetry so a future "simplify the
// cascade" refactor can't quietly collapse them back into a
// single gate.

import (
	"testing"
	"time"
)

// TestDepGate_SubmittedSatisfiesPureDependsOn is the load-
// bearing reviewer-readiness invariant. A pending downstream
// with depends_on parent and NO reads_artifacts moves to READY
// the moment the parent transitions to SUBMITTED — before the
// merge confirms, before /merges fires. Without this, the
// review-pattern (reviewer.depends_on = upstream.target) would
// deadlock: the reviewer can't claim, the upstream can't get
// reviewed, the merge that would flip ACCEPTED never happens.
func TestDepGate_SubmittedSatisfiesPureDependsOn(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	alice := createTestCitizen(t, s, "alice", "tok-dg1")

	// Drive the upstream task through claim+submit so it lands
	// in SUBMITTED via the production write path
	// (applyRecordSubmission, post-Phase-8.3).
	parentID := makeTaskWithAction(t, s, runID, "parent", "answer", TaskReady)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: parentID, CitizenID: alice, Deadline: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatalf("claim parent: %v", err)
	}

	// Insert the pure-depends_on downstream BEFORE the parent
	// submits. Action=answer (not review) and no reads_artifacts
	// — this is the structural-dep case, NOT the content-read
	// case. State=PENDING so the cascade has work to do.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:downstream", RunID: runID, Seq: 99, TaskDefID: "downstream",
		Action:    "answer",
		ResultType: "text",
		State:     TaskPending,
		DependsOn: parentID,
		AssignTo:  `["alice"]`,
		CreatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Submit the parent. RecordSubmission writes state='submitted'
	// (Phase 8.3) and AppendCascade fires the readiness sweep.
	// Pre-Phase-8.3 the parent went to ACCEPTED here; the dep
	// gate hadn't needed SUBMITTED in its satisfied set.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		RecordSubmission{
			TaskID: parentID, CitizenID: alice,
			CommitSHA: "deadbeef", TokensUsed: 1, EstimatedTokens: 1,
		},
		UpdateReadyTasks{RunID: runID},
	}}); err != nil {
		t.Fatalf("submit parent: %v", err)
	}

	// Parent should now be SUBMITTED (not ACCEPTED — that
	// happens later, via inline acceptTask or /merges-driven
	// acceptTask depending on whether a merge view fires).
	parent, _ := s.GetTask(parentID)
	if parent == nil {
		t.Fatal("parent missing")
	}
	if parent.State != TaskSubmitted {
		t.Fatalf("parent.State = %q, want submitted (Phase 8.3 deferred-accept)", parent.State)
	}

	// Downstream MUST be READY. This is the assertion the
	// reviewer is waiting for: depends_on satisfied at SUBMITTED
	// means the reviewer can now claim and review the upstream
	// during its merge-pending window.
	downstream, _ := s.GetTask("1:1:downstream")
	if downstream == nil {
		t.Fatal("downstream missing")
	}
	if downstream.State != TaskReady {
		t.Fatalf("downstream.State = %q, want ready (depends_on must consider SUBMITTED parent satisfied — Phase 8.3 dep gate)", downstream.State)
	}
}

// TestDepGate_SubmittedDoesNotSatisfyReadsArtifacts pins the
// stricter half of the dual-tier gate. A pending downstream
// with reads_artifacts depending on a SUBMITTED writer's output
// MUST stay in PENDING until the writer reaches ACCEPTED — the
// merge has to confirm before the artifact's content is on the
// run branch. Without this, downstream readers fan out against
// commits still living only on topic branches; that's the
// silent-cascade-stall failure mode driving Phase 8.3.
//
// The artifact-index row exists at submit time (inserted via
// applyMoveArtifact) but applyUpdateReadyTasks's per-path
// existence query gates on the writer's state via NOT EXISTS,
// hiding SUBMITTED-writer rows from the readiness sweep.
func TestDepGate_SubmittedDoesNotSatisfyReadsArtifacts(t *testing.T) {
	s := newTestStore(t)
	runID := createTestRun(t, s)
	run, _ := s.GetRun(runID)
	projectID := run.ProjectID
	alice := createTestCitizen(t, s, "alice", "tok-dg2")

	// Writer task → SUBMITTED via the production path.
	writerID := makeTaskWithAction(t, s, runID, "writer", "answer", TaskReady)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetClaim{TaskID: writerID, CitizenID: alice, Deadline: time.Now().Add(time.Hour)},
	}}); err != nil {
		t.Fatalf("claim writer: %v", err)
	}
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		RecordSubmission{
			TaskID: writerID, CitizenID: alice,
			CommitSHA: "deadbeef", TokensUsed: 1, EstimatedTokens: 1,
		},
	}}); err != nil {
		t.Fatalf("submit writer: %v", err)
	}

	// Insert the artifact-index row pointing at the SUBMITTED
	// writer. Production lands this via accept.go's
	// ArtifactMutations apply at submit time (which Phase 8.3
	// retains — it's the read SIDE that gates, not the write
	// side). Mirroring it inline here keeps the test's scope on
	// the gate, not on the upsert path.
	now := time.Now().UTC().Truncate(time.Second)
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		MoveArtifact{Artifact: ArtifactRecord{
			ProjectID:  projectID,
			Branch:     "main",
			Path:       "out/payload.md",
			LastTaskID: writerID,
			LastWriter: alice,
			LastRunID:  runID,
			CommitSHA:  "deadbeef",
			Tracked:    true,
			CreatedAt:  now, UpdatedAt: now,
		}},
	}}); err != nil {
		t.Fatalf("upsert artifact: %v", err)
	}

	// Reader task: reads_artifacts the writer's output. State=
	// PENDING. depends_on left empty so ONLY the artifact-state
	// gate is in play.
	if err := helperCreateTask(s, &TaskRecord{
		ID: "1:1:reader", RunID: runID, Seq: 99, TaskDefID: "reader",
		Action:         "answer",
		ResultType:     "text",
		State:          TaskPending,
		ReadsArtifacts: `["out/payload.md"]`,
		AssignTo:       `["alice"]`,
		CreatedAt:      time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	// Run the cascade. The artifact-state gate must hide the
	// SUBMITTED-writer row, leaving the reader's reads_artifacts
	// dep unsatisfied → reader stays PENDING.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		UpdateReadyTasks{RunID: runID},
	}}); err != nil {
		t.Fatalf("cascade pre-accept: %v", err)
	}
	reader, _ := s.GetTask("1:1:reader")
	if reader == nil {
		t.Fatal("reader missing")
	}
	if reader.State != TaskPending {
		t.Fatalf("reader.State = %q, want pending (artifact-state gate must hide SUBMITTED-writer rows — Phase 8.3 silent-cascade-stall fix)", reader.State)
	}

	// Now flip the writer to ACCEPTED. The artifact row
	// becomes visible to the gate, the cascade promotes the
	// reader. This pins the SUBMITTED → ACCEPTED transition as
	// the unblocking moment for content-reading downstream.
	if _, err := s.ApplyPlan(Plan{Mutations: []Mutation{
		SetTaskState{TaskID: writerID, NewState: TaskAccepted},
		UpdateReadyTasks{RunID: runID},
	}}); err != nil {
		t.Fatalf("accept writer: %v", err)
	}
	reader, _ = s.GetTask("1:1:reader")
	if reader.State != TaskReady {
		t.Fatalf("reader.State = %q, want ready (writer ACCEPTED unblocks artifact-state gate)", reader.State)
	}
}
