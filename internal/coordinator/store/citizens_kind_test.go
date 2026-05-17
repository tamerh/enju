package store

// contract tests — operator/model design migration.
// Pin the additive schema change for citizen.kind + parent_id:
//
//   - Existing humans default to kind='human', parent_id=NULL —
//     so the migration is safe to run on a populated DB.
//   - Reads through every lookup path (token, id, username) carry
//     the new fields, so callers depending on CitizenRecord don't
//     see zero-valued defaults from a missing scan.
//   - Tokens still authenticate after migration (the load-bearing
//     property — operator/model design must not break existing humans).
//
// See docs/operator-model-design.md.

import (
	"testing"
	"time"
)

// TestCitizenKindDefaultsToHuman is the migration safety test. Every
// citizen created via the existing CreateCitizen path (which doesn't
// know about kind) must come back with kind='human' on read. If this
// fails, the column default is wrong or scanCitizen is reading a
// different column.
func TestCitizenKindDefaultsToHuman(t *testing.T) {
	s := newTestStore(t)
	id := createTestCitizen(t, s, "tamer", "tok-tamer")

	for _, lookup := range []struct {
		name string
		get  func() (*CitizenRecord, error)
	}{
		{"by id", func() (*CitizenRecord, error) { return s.GetCitizen(id) }},
		{"by username", func() (*CitizenRecord, error) { return s.GetCitizenByUsername("tamer") }},
		{"by token", func() (*CitizenRecord, error) { return s.GetCitizenByToken("tok-tamer") }},
	} {
		t.Run(lookup.name, func(t *testing.T) {
			c, err := lookup.get()
			if err != nil {
				t.Fatalf("lookup error: %v", err)
			}
			if c == nil {
				t.Fatal("citizen not found")
			}
			if c.Kind != "human" {
				t.Errorf("kind=%q, want %q (column default should backfill existing inserts)", c.Kind, "human")
			}
			if c.ParentID != nil {
				t.Errorf("parent_id=%v, want nil for human", *c.ParentID)
			}
		})
	}
}

// TestCitizenKindReadAfterDirectInsert covers the bot/model insert
// shape that bot/model registration uses. CreateCitizen doesn't expose
// kind or parent_id yet, so this test goes through the raw db.Exec
// path the same way later phases will. Pinning that the scan path
// reads both fields correctly means 1.5 and 1.3 won't silently lose
// data when they start using these columns.
func TestCitizenKindReadAfterDirectInsert(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")

	// Insert a bot owned by tamer. future helpers may wrap this in a
	// CreateBot helper; for now we exercise the raw schema.
	now := time.Now()
	_, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, ?, ?)`,
		"claude-tamer-bot", "Claude (tamer's bot)", "tok-bot", now, now, string(CitizenKindBot), humanID,
	)
	if err != nil {
		t.Fatalf("insert bot: %v", err)
	}

	bot, err := s.GetCitizenByUsername("claude-tamer-bot")
	if err != nil || bot == nil {
		t.Fatalf("bot lookup: %v / %v", err, bot)
	}
	if bot.Kind != CitizenKindBot {
		t.Errorf("kind=%q, want %q", bot.Kind, CitizenKindBot)
	}
	if bot.ParentID == nil {
		t.Fatal("parent_id is nil; want pointer to humanID")
	}
	if *bot.ParentID != humanID {
		t.Errorf("parent_id=%d, want %d", *bot.ParentID, humanID)
	}

	// And a model citizen — the migration already seeds the popular
	// catalog (claude-opus-4-7, gpt-4o, etc.) so this test uses
	// a name that's not in the seed list to avoid collision.
	// The point is just to pin the read-path mechanics for
	// kind='model' rows; the seed itself is covered by separate
	// separate model-catalog tests.
	_, err = s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind)
		 VALUES (?, ?, '', 'citizen', ?, 0, ?, ?, 'model')`,
		"my-custom-llm", "My Custom LLM", "model:my-custom-llm", now, now,
	)
	if err != nil {
		t.Fatalf("insert model: %v", err)
	}
	model, err := s.GetCitizenByUsername("my-custom-llm")
	if err != nil || model == nil {
		t.Fatalf("model lookup: %v / %v", err, model)
	}
	if model.Kind != "model" {
		t.Errorf("kind=%q, want %q", model.Kind, "model")
	}
	if model.ParentID != nil {
		t.Errorf("parent_id=%v, want nil for model (catalog entry, not owned)", *model.ParentID)
	}
}

// TestCreateCitizenHonorsKindAndParent closes the gap the original
// test comment flagged but the original code didn't fix: when
// CreateCitizen is called with Kind='bot' and ParentID set (which
// is what enju_register_bot does), the INSERT must propagate both
// fields rather than letting the schema's column defaults take
// over.
//
// Without this test, the bug ships silently: registered "bots"
// land as kind='human' parent_id=NULL, ListBotsByParent returns
// empty, and the entire bot tooling chain (list, revoke, the
// requireModelForBot constraint) becomes dead code. The user
// can't tell anything is wrong because everything compiles and
// the JSON response looks plausible.
//
// This is the load-bearing write-side counterpart to
// TestCitizenKindReadAfterDirectInsert (which tested only the
// scan path via raw db.Exec).
func TestCreateCitizenHonorsKindAndParent(t *testing.T) {
	s := newTestStore(t)
	humanID := createTestCitizen(t, s, "tamer", "tok-tamer")

	// Register a bot the way enju_register_bot does.
	now := time.Now()
	botID, err := helperCreateCitizen(s, &CitizenRecord{
		Username:     "claude-tamer-bot",
		Name:         "Tamer's Claude bot",
		Token:        "tok-bot",
		RegisteredAt: now,
		LastSeen:     now,
		Kind:         CitizenKindBot,
		ParentID:     &humanID,
	})
	if err != nil {
		t.Fatalf("CreateCitizen: %v", err)
	}

	// Read back through the username path (what handleListMyBots
	// effectively does via ListBotsByParent).
	bot, err := s.GetCitizenByUsername("claude-tamer-bot")
	if err != nil || bot == nil {
		t.Fatalf("lookup: %v / %v", err, bot)
	}
	if bot.Kind != CitizenKindBot {
		t.Errorf("kind=%q, want %q (CreateCitizen dropped Kind — schema default took over)", bot.Kind, CitizenKindBot)
	}
	if bot.ParentID == nil {
		t.Fatal("parent_id is nil — CreateCitizen dropped ParentID")
	}
	if *bot.ParentID != humanID {
		t.Errorf("parent_id=%d, want %d", *bot.ParentID, humanID)
	}

	// And the listing query — this is what enju_list_my_bots
	// uses, so failing here = the empty-list-forever bug.
	bots, err := s.ListBotsByParent(humanID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bots) != 1 {
		t.Fatalf("ListBotsByParent returned %d bots, want 1; CreateCitizen wrote the wrong parent_id", len(bots))
	}
	if bots[0].ID != botID {
		t.Errorf("bot id=%d, want %d", bots[0].ID, botID)
	}
}

// TestCreateCitizenDefaultsKindToHuman pins the conditional default
// in CreateCitizen: when callers don't set Kind (the existing
// register-citizen path uses CitizenRecord with Kind=""), the row
// must land as kind='human'. Without this, an empty Kind would
// either fail a NOT NULL constraint or insert literal "" — both
// regressions.
func TestCreateCitizenDefaultsKindToHuman(t *testing.T) {
	s := newTestStore(t)
	id := createTestCitizen(t, s, "alice", "tok-alice")

	c, err := s.GetCitizen(id)
	if err != nil || c == nil {
		t.Fatalf("lookup: %v / %v", err, c)
	}
	if c.Kind != "human" {
		t.Errorf("kind=%q, want %q (Kind=\"\" should default to human)", c.Kind, "human")
	}
}

// TestExistingHumansAuthenticateAfterMigration is the key load-
// bearing assertion: the migration must NOT break the existing
// auth path. Token lookup is what every authenticated request
// goes through (router.go middleware); if this regresses, every
// human user is locked out on the next coordinator restart.
func TestExistingHumansAuthenticateAfterMigration(t *testing.T) {
	s := newTestStore(t)
	createTestCitizen(t, s, "tamer", "tok-tamer-auth")

	c, err := s.GetCitizenByToken("tok-tamer-auth")
	if err != nil {
		t.Fatalf("GetCitizenByToken error: %v", err)
	}
	if c == nil {
		t.Fatal("token did not resolve — auth broken after migration")
	}
	if c.Username != "tamer" {
		t.Errorf("username=%q, want tamer", c.Username)
	}
	if c.Kind != "human" {
		t.Errorf("kind=%q, want human (default for existing rows)", c.Kind)
	}
}
