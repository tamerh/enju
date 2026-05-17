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
//   - model is OPTIONAL attribution, never forced by kind. Every
//     combination is allowed; the model is just stored (or NULL):
//       human + ""    → allowed (hand-review / script — no LLM)
//       human + named → allowed (human at keyboard with an LLM)
//       agent + ""    → allowed (a script / lint agent — no LLM)
//       agent + named → allowed (an LLM agent)
//     agent-ness does NOT imply an LLM ran; there is no claim-time
//     or submit-time model requirement.

import (
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

// TestAgentMayActWithoutModel — the model is OPTIONAL attribution,
// never forced by kind. An agent can run a script (a lint-agent, a
// compute step) where no LLM produced the work; agent-ness does
// not imply an LLM ran. Claim AND submit with an empty model must
// be accepted, and the stored model stays empty (NULL) — same as a
// compute task. This pins the removal of the old "an agent must
// name a model" rule (it falsely credited script work to a model).
func TestAgentMayActWithoutModel(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")

	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', 0, ?, ?, ?, ?)`,
		"lint-agent", "Lint Agent (script handler)", now, now, string(CitizenKindAgent), humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	stageTaskForClaim(t, s, "t1")

	// Claim with NO model — accepted (a script agent has none).
	if err := applyClaimSubmit(t, s, "t1", botID, ""); err != nil {
		t.Fatalf("agent claim+submit with empty model must be accepted, got: %v", err)
	}

	subs, _ := s.ListVoteSubmissions("t1")
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].Model != "" {
		t.Errorf("script agent's model = %q, want empty (no LLM produced the work)", subs[0].Model)
	}
	if subs[0].CitizenID != botID {
		t.Errorf("operator = %d, want lint-agent %d", subs[0].CitizenID, botID)
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
		`INSERT INTO citizens (username, name, email, role, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', 0, ?, ?, ?, ?)`,
		"developer-bot", "Developer Agent", now, now, string(CitizenKindAgent), humanID,
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
