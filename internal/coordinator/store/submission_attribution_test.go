package store

// contract tests — submission attribution (operator,
// model). The properties this milestone pins:
//
//   - task_claims.citizen_id is the operator (existing); model_id
//     is the new column carrying which LLM produced the words.
//   - SetClaim and RecordSubmission carry ModelID through to the
//     row, both on insert (claim) and on update (submit).
//   - The kind-based constraint applies at apply time (since
//     SQLite CHECK can't reference citizens.kind):
//       human + nil model_id  → allowed (hand-review case)
//       human + non-nil model → allowed (typical: human at keyboard with Claude)
//       bot   + nil model_id  → REJECTED (bots can't think alone)
//       bot   + non-nil model → allowed
//   - The constraint fires on BOTH paths (claim and submit), so a
//     bot that lies about its model gets caught regardless of when
//     it tries to.
//
// See docs/operator-model-design.md.

import (
	"strings"
	"testing"
	"time"
)

// applyClaimSubmit drives a SetClaim + RecordSubmission cycle for
// the test task `taskID`, against the given operator/model. Returns
// the ApplyPlan error from whichever step fails (or nil on full
// success). Centralizing this lets each test focus on the (operator
// kind, model nil-ness) matrix without re-staging plumbing.
func applyClaimSubmit(t *testing.T, s *Store, taskID string, operatorID int64, modelID *int64) error {
	t.Helper()

	// Claim.
	if _, err := s.ApplyPlan(Plan{
		Mutations: []Mutation{
			SetClaim{
				TaskID:    taskID,
				CitizenID: operatorID,
				Deadline:  time.Now().Add(time.Hour),
				ModelID:   modelID,
			},
		},
	}); err != nil {
		return err
	}

	// Submit.
	if _, err := s.ApplyPlan(Plan{
		Mutations: []Mutation{
			RecordSubmission{
				TaskID:     taskID,
				CitizenID:  operatorID,
				ResultPath: "runs/1/test/result.md",
				CommitSHA:  "abc1234567890",
				ModelID:    modelID,
			},
		},
	}); err != nil {
		return err
	}
	return nil
}

// stageTaskForClaim creates the bare minimum project + run + task
// scaffolding that a SetClaim mutation needs. Returns the task
// ID. Done with raw SQL so each test gets a clean slate without
// pulling in the whole engine.
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

// TestHumanCanSubmitWithoutModel — the explicit design carve-out:
// a human reviewer who hand-decides without invoking an LLM submits
// with model_id=nil. No error; row stored cleanly.
func TestHumanCanSubmitWithoutModel(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	stageTaskForClaim(t, s, "t1")

	if err := applyClaimSubmit(t, s, "t1", humanID, nil); err != nil {
		t.Fatalf("human + nil model should succeed, got: %v", err)
	}

	// Verify the row landed with model_id NULL.
	subs, err := s.ListVoteSubmissions("t1")
	if err != nil {
		t.Fatal(err)
	}
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].ModelID != nil {
		t.Errorf("model_id=%d, want nil for unaided human", *subs[0].ModelID)
	}
	if subs[0].CitizenID != humanID {
		t.Errorf("operator (citizen_id)=%d, want %d", subs[0].CitizenID, humanID)
	}
}

// TestHumanWithModelRecordsBoth — typical case: human at keyboard
// using Claude. Both citizen_id (operator) and model_id are
// populated and round-trip through the read path.
func TestHumanWithModelRecordsBoth(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")

	// Look up Opus from the seeded catalog.
	opus, err := s.GetCitizenByUsername("claude-opus-4-7")
	if err != nil || opus == nil {
		t.Fatalf("seed lookup: %v / %v", err, opus)
	}
	stageTaskForClaim(t, s, "t1")

	if err := applyClaimSubmit(t, s, "t1", humanID, &opus.ID); err != nil {
		t.Fatalf("human + Opus should succeed: %v", err)
	}

	subs, _ := s.ListVoteSubmissions("t1")
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].ModelID == nil || *subs[0].ModelID != opus.ID {
		t.Errorf("model_id=%v, want %d (Opus)", subs[0].ModelID, opus.ID)
	}
}

// TestBotRequiresModelOnClaim — the load-bearing constraint. A bot
// operator MUST name a model. The check fires at SetClaim (the
// earliest enforcement point) so a bot can't even start work
// without declaring its model.
func TestBotRequiresModelOnClaim(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")

	// Insert a bot owned by tamer (future helpers may wrap this in a
	// proper helper; for now raw SQL).
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, 'bot', ?)`,
		"claude-tamer-bot", "Tamer's Claude bot", "tok-bot", now, now, humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	stageTaskForClaim(t, s, "t1")

	// Bot tries to claim without naming a model → must reject.
	_, err = s.ApplyPlan(Plan{
		Mutations: []Mutation{
			SetClaim{
				TaskID:    "t1",
				CitizenID: botID,
				Deadline:  time.Now().Add(time.Hour),
				ModelID:   nil,
			},
		},
	})
	if err == nil {
		t.Fatal("bot claim with nil model_id was accepted; expected rejection")
	}
	if !strings.Contains(err.Error(), "bot") || !strings.Contains(err.Error(), "model_id is required") {
		t.Errorf("error wording should mention bot + model requirement; got: %v", err)
	}
}

// TestBotRequiresModelOnSubmit — defense in depth. Even if a
// human-with-model claims a task and then the bot somehow tries
// to submit (shouldn't happen, but the constraint must hold),
// applyRecordSubmission also rejects. Tests claim-time AND
// submit-time enforcement so a bot can't slip through either.
func TestBotRequiresModelOnSubmit(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, 'bot', ?)`,
		"reviewer-bot", "Reviewer Bot", "tok-rbot", now, now, humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	stageTaskForClaim(t, s, "t1")

	// First, claim WITH a model (Opus). the apply path lets that
	// land cleanly.
	opus, _ := s.GetCitizenByUsername("claude-opus-4-7")
	if _, err := s.ApplyPlan(Plan{
		Mutations: []Mutation{
			SetClaim{TaskID: "t1", CitizenID: botID, Deadline: time.Now().Add(time.Hour), ModelID: &opus.ID},
		},
	}); err != nil {
		t.Fatalf("setup claim with model: %v", err)
	}

	// Now submit WITHOUT a model — the constraint must catch this
	// too (defense in depth: bot can't drop attribution mid-flow).
	_, err = s.ApplyPlan(Plan{
		Mutations: []Mutation{
			RecordSubmission{
				TaskID:     "t1",
				CitizenID:  botID,
				ResultPath: "runs/1/test/result.md",
				CommitSHA:  "abc",
				ModelID:    nil,
			},
		},
	})
	if err == nil {
		t.Fatal("bot submit with nil model_id was accepted; expected rejection")
	}
	if !strings.Contains(err.Error(), "bot") {
		t.Errorf("error should name the bot constraint; got: %v", err)
	}
}

// TestBotWithModelSucceeds — the happy path for bots: declare the
// model, both phases apply cleanly, model_id round-trips through
// the read path.
func TestBotWithModelSucceeds(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, 'bot', ?)`,
		"developer-bot", "Developer Bot", "tok-dbot", now, now, humanID,
	)
	if err != nil {
		t.Fatal(err)
	}
	botID, _ := res.LastInsertId()
	sonnet, _ := s.GetCitizenByUsername("claude-sonnet-4-6")
	stageTaskForClaim(t, s, "t1")

	if err := applyClaimSubmit(t, s, "t1", botID, &sonnet.ID); err != nil {
		t.Fatalf("bot + Sonnet should succeed: %v", err)
	}

	subs, _ := s.ListVoteSubmissions("t1")
	if len(subs) != 1 {
		t.Fatalf("got %d submissions, want 1", len(subs))
	}
	if subs[0].CitizenID != botID {
		t.Errorf("operator=%d, want bot id %d", subs[0].CitizenID, botID)
	}
	if subs[0].ModelID == nil || *subs[0].ModelID != sonnet.ID {
		t.Errorf("model_id=%v, want sonnet id %d", subs[0].ModelID, sonnet.ID)
	}
}
