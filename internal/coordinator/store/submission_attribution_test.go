package store

// Contract tests — submission attribution (operator, model).
// The properties pinned here:
//
//   - task_claims.citizen_id is the operator; `model` is the
//     normalized model-name LABEL that produced the words. It is
//     a plain string, NOT a citizen reference — a model has no
//     identity.
//   - SetClaim and RecordSubmission carry the model string through
//     to the row, both on insert (claim) and on update (submit).
//   - The kind-based constraint applies at apply time (SQLite
//     CHECK can't conditionally read citizens.kind):
//       human + ""    → allowed (hand-review / script case)
//       human + named → allowed (human at keyboard with an LLM)
//       agent + ""    → REJECTED (an agent can't act with no model)
//       agent + named → allowed
//   - The constraint fires on BOTH paths (claim and submit), so an
//     agent that drops its model gets caught regardless of when.

import (
	"strings"
	"testing"
	"time"
)

// applyClaimSubmit drives a SetClaim + RecordSubmission cycle for
// the test task `taskID`, against the given operator/model string.
// Returns the ApplyPlan error from whichever step fails (or nil on
// full success).
func applyClaimSubmit(t *testing.T, s *Store, taskID string, operatorID int64, model string) error {
	t.Helper()

	if _, err := s.ApplyPlan(Plan{
		Mutations: []Mutation{
			SetClaim{
				TaskID:    taskID,
				CitizenID: operatorID,
				Deadline:  time.Now().Add(time.Hour),
				Model:     model,
			},
		},
	}); err != nil {
		return err
	}

	if _, err := s.ApplyPlan(Plan{
		Mutations: []Mutation{
			RecordSubmission{
				TaskID:     taskID,
				CitizenID:  operatorID,
				ResultPath: "runs/1/test/result.md",
				CommitSHA:  "abc1234567890",
				Model:      model,
			},
		},
	}); err != nil {
		return err
	}
	return nil
}

// stageTaskForClaim creates the bare minimum project + run + task
// scaffolding that a SetClaim mutation needs. Raw SQL so each test
// gets a clean slate without pulling in the whole engine.
func stageTaskForClaim(t *testing.T, s *Store, taskID string) {
	t.Helper()
	now := time.Now()
	if _, err := s.db.Exec(
		`INSERT INTO projects (name, created_at, updated_at) VALUES (?, ?, ?)`,
		"proj-"+taskID, now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO runs (id, project_id, seq, name, state, created_at, updated_at) VALUES (1, 1, 1, 'r', 'active', ?, ?)`,
		now, now,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(
		`INSERT INTO tasks (id, run_id, task_def_id, action, state, citizens, created_at) VALUES (?, 1, 't', 'answer', 'ready', 1, ?)`,
		taskID, now,
	); err != nil {
		t.Fatal(err)
	}
}

// TestHumanCanSubmitWithoutModel — the explicit carve-out: a human
// reviewer who hand-decides without invoking an LLM (or a script
// task) submits with an empty model. No error; row stored cleanly
// with model NULL.
func TestHumanCanSubmitWithoutModel(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	stageTaskForClaim(t, s, "t1")

	if err := applyClaimSubmit(t, s, "t1", humanID, ""); err != nil {
		t.Fatalf("human + empty model should succeed, got: %v", err)
	}

	subs, err := s.ListVoteSubmissions("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].Model != "" {
		t.Errorf("model=%q, want empty for unaided human", subs[0].Model)
	}
	if subs[0].CitizenID != humanID {
		t.Errorf("operator (citizen_id)=%d, want %d", subs[0].CitizenID, humanID)
	}
}

// TestHumanWithModelRecordsBoth — typical case: human at keyboard
// using Claude. Both citizen_id (operator) and the model string
// round-trip through the read path. The model is a literal label,
// not a citizen — no catalog lookup needed.
func TestHumanWithModelRecordsBoth(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	stageTaskForClaim(t, s, "t1")

	if err := applyClaimSubmit(t, s, "t1", humanID, "claude-opus-4-7"); err != nil {
		t.Fatalf("human + model should succeed: %v", err)
	}

	subs, _ := s.ListVoteSubmissions("t1")
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].Model != "claude-opus-4-7" {
		t.Errorf("model=%q, want %q", subs[0].Model, "claude-opus-4-7")
	}
}

// TestAgentRequiresModelOnClaim — the load-bearing constraint. An
// agent operator MUST name a model. The check fires at SetClaim
// (the earliest point) so an agent can't even start work without
// declaring its model.
func TestAgentRequiresModelOnClaim(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")

	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, ?, ?)`,
		"claude-tamer-bot", "Tamer's Claude agent", "tok-bot", now, now, string(CitizenKindBot), humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	stageTaskForClaim(t, s, "t1")

	_, err = s.ApplyPlan(Plan{
		Mutations: []Mutation{
			SetClaim{
				TaskID:    "t1",
				CitizenID: botID,
				Deadline:  time.Now().Add(time.Hour),
				Model:     "",
			},
		},
	})
	if err == nil {
		t.Fatal("agent claim with empty model was accepted; expected rejection")
	}
	if !strings.Contains(err.Error(), "agent") || !strings.Contains(err.Error(), "model is required") {
		t.Errorf("error wording should mention agent + model requirement; got: %v", err)
	}
}

// TestAgentRequiresModelOnSubmit — defense in depth. Even if the
// claim somehow carried a model, applyRecordSubmission also rejects
// an empty model for an agent, so an agent can't drop attribution
// mid-flow.
func TestAgentRequiresModelOnSubmit(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, ?, ?)`,
		"reviewer-bot", "Reviewer Agent", "tok-rbot", now, now, string(CitizenKindBot), humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	stageTaskForClaim(t, s, "t1")

	// Claim WITH a model — that lands cleanly.
	if _, err := s.ApplyPlan(Plan{
		Mutations: []Mutation{
			SetClaim{TaskID: "t1", CitizenID: botID, Deadline: time.Now().Add(time.Hour), Model: "claude-opus-4-7"},
		},
	}); err != nil {
		t.Fatalf("setup claim with model: %v", err)
	}

	// Submit WITHOUT a model — the constraint must catch this too.
	_, err = s.ApplyPlan(Plan{
		Mutations: []Mutation{
			RecordSubmission{
				TaskID:     "t1",
				CitizenID:  botID,
				ResultPath: "runs/1/test/result.md",
				CommitSHA:  "abc",
				Model:      "",
			},
		},
	})
	if err == nil {
		t.Fatal("agent submit with empty model was accepted; expected rejection")
	}
	if !strings.Contains(err.Error(), "agent") {
		t.Errorf("error should name the agent constraint; got: %v", err)
	}
}

// TestAgentWithModelSucceeds — the happy path for agents: declare
// the model, both phases apply cleanly, the model string
// round-trips through the read path.
func TestAgentWithModelSucceeds(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, ?, ?)`,
		"developer-bot", "Developer Agent", "tok-dbot", now, now, string(CitizenKindBot), humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	stageTaskForClaim(t, s, "t1")

	if err := applyClaimSubmit(t, s, "t1", botID, "claude-sonnet-4-6"); err != nil {
		t.Fatalf("agent + model should succeed: %v", err)
	}

	subs, _ := s.ListVoteSubmissions("t1")
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].CitizenID != botID {
		t.Errorf("operator=%d, want agent id %d", subs[0].CitizenID, botID)
	}
	if subs[0].Model != "claude-sonnet-4-6" {
		t.Errorf("model=%q, want %q", subs[0].Model, "claude-sonnet-4-6")
	}
}
