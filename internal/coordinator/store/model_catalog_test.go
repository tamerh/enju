package store

// contract tests — model citizens are catalog entries.
// The properties this milestone pins:
//
//   - The hand-curated seed lands automatically on first migration
//     and is idempotent on re-run, so a coordinator restart doesn't
//     duplicate or perturb the catalog.
//   - Model citizens are kind='model', have no parent, and DO NOT
//     authenticate via their placeholder token (the placeholder
//     exists only to satisfy the legacy NOT NULL UNIQUE constraint
//     on citizens.token; the auth path goes through the tokens
//     tokens table and we deliberately don't insert a
//     tokens row for them).
//   - Users can register new models via CreateModelCitizen with
//     the standard "username taken" error on conflict.
//   - ListModelCitizens returns only kind='model' rows so the
//     catalog browse / dropdown stays scoped.
//
// See docs/operator-model-design.md.

import (
	"strings"
	"testing"
)

// TestSeedCatalogPopulatedOnFirstMigration verifies the migration
// inserted every row from modelCatalogSeed. This is the load-bearing
// property — submit-attribution will reference these
// rows, and the seed list IS the public surface citizens see when
// they choose a model. Drift between the seed and what's in the DB
// is silent and bad.
func TestSeedCatalogPopulatedOnFirstMigration(t *testing.T) {
	s := newTestStore(t)

	for _, m := range modelCatalogSeed {
		c, err := s.GetCitizenByUsername(m.Username)
		if err != nil {
			t.Fatalf("lookup %s: %v", m.Username, err)
		}
		if c == nil {
			t.Errorf("seed model %q missing from catalog after migration", m.Username)
			continue
		}
		if c.Kind != "model" {
			t.Errorf("%s: kind=%q, want %q", m.Username, c.Kind, "model")
		}
		if c.Name != m.Name {
			t.Errorf("%s: name=%q, want %q", m.Username, c.Name, m.Name)
		}
		if c.ParentID != nil {
			t.Errorf("%s: parent_id=%v, want nil for catalog model", m.Username, *c.ParentID)
		}
	}
}

// TestSeedIsIdempotent — running initSchema() on a populated DB must
// not crash, duplicate, or modify catalog rows. The implementation
// uses an existence check in upsertModelCitizen rather than INSERT
// OR IGNORE; this test pins that behavior so a future refactor
// doesn't accidentally introduce churn on every coordinator boot.
func TestSeedIsIdempotent(t *testing.T) {
	s := newTestStore(t)

	// First migration ran via newTestStore. Run again manually to
	// simulate a coordinator restart on the same DB file.
	if err := s.seedModelCitizens(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}

	// Count check — exactly one row per seed entry, no dupes.
	for _, m := range modelCatalogSeed {
		var count int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM citizens WHERE username = ? AND kind = 'model'`,
			m.Username,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%s appears %d times after re-seed; want exactly 1", m.Username, count)
		}
	}
}

// TestModelPlaceholderTokenDoesNotAuthenticate is the security
// property: the "model:<username>" placeholder exists only to
// satisfy the legacy citizens.token NOT NULL UNIQUE constraint.
// It MUST NOT match the auth path. If this regresses, anyone who
// can guess a model name (everyone — the catalog is public)
// becomes that model citizen and could submit on its behalf,
// which would be a confused-deputy attack waiting to happen.
func TestModelPlaceholderTokenDoesNotAuthenticate(t *testing.T) {
	s := newTestStore(t)

	for _, m := range modelCatalogSeed {
		placeholder := "model:" + m.Username
		c, err := s.GetCitizenByToken(placeholder)
		if err != nil {
			t.Fatalf("lookup %s: %v", placeholder, err)
		}
		if c != nil {
			t.Errorf("placeholder token %q authenticated as %s — auth bypass!", placeholder, c.Username)
		}
	}
}

// TestCreateModelCitizenAddsToCatalog covers the user-extension
// path that enju_register_model will call once the MCP tool layer wires
// the MCP tool. The store-level helper is what the API handler
// will sit on; pinning its semantics now lets the tool work
// land cleanly.
func TestCreateModelCitizenAddsToCatalog(t *testing.T) {
	s := newTestStore(t)

	id, err := helperCreateModelCitizen(s, "ollama-llama-3-1-70b-local", "Llama 3.1 70B (local)")
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if id == 0 {
		t.Error("returned id is 0")
	}

	// Round-trip through the username lookup.
	c, err := s.GetCitizenByUsername("ollama-llama-3-1-70b-local")
	if err != nil || c == nil {
		t.Fatalf("lookup after register: %v / %v", err, c)
	}
	if c.Kind != "model" {
		t.Errorf("kind=%q, want model", c.Kind)
	}
	if c.Name != "Llama 3.1 70B (local)" {
		t.Errorf("name=%q", c.Name)
	}
}

// TestCreateModelCitizenRejectsDuplicate — the seed already
// contains "claude-opus-4-7"; trying to register it again must
// fail with a clear error. Same behavior as duplicate-username
// for humans, just on the model surface.
func TestCreateModelCitizenRejectsDuplicate(t *testing.T) {
	s := newTestStore(t)

	_, err := helperCreateModelCitizen(s, "claude-opus-4-7", "Claude Opus 4.7")
	if err == nil {
		t.Fatal("expected duplicate-username error, got nil")
	}
	if !strings.Contains(err.Error(), "already taken") {
		t.Errorf("error wording: %v", err)
	}
}

// TestCreateModelCitizenRejectsBadUsername reuses the human
// validation regex (lowercase alphanumerics + hyphen). Future
// phases may relax this for model citizens (slashes, dots — see
// design doc) but for now the regex applies uniformly.
func TestCreateModelCitizenRejectsBadUsername(t *testing.T) {
	s := newTestStore(t)
	bad := []string{"With-Caps", "has spaces", "trailing-", "-leading", "has.dot"}
	for _, name := range bad {
		if _, err := helperCreateModelCitizen(s, name, "x"); err == nil {
			t.Errorf("%q registered, expected validation error", name)
		}
	}
}

// TestListModelCitizensReturnsOnlyModels — the catalog browse
// must not leak human or bot citizens. Defensive sanity check
// against a future query bug.
func TestListModelCitizensReturnsOnlyModels(t *testing.T) {
	s := newTestStore(t)

	// Add a human and a bot to make the test meaningful.
	createTestCitizen(t, s, "tamer", "tok-tamer-listcheck")
	humanID := createTestCitizen(t, s, "alice", "tok-alice-listcheck")
	if _, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen, kind, parent_id)
		 VALUES (?, ?, '', 'citizen', ?, 0, datetime('now'), datetime('now'), 'bot', ?)`,
		"alice-bot", "Alice's bot", "tok-alice-bot", humanID,
	); err != nil {
		t.Fatal(err)
	}

	models, err := s.ListModelCitizens()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(models) != len(modelCatalogSeed) {
		t.Errorf("got %d models, want %d (matches seed exactly — no human/bot leak)",
			len(models), len(modelCatalogSeed))
	}
	for _, m := range models {
		if m.Kind != "model" {
			t.Errorf("ListModelCitizens returned kind=%q for %s", m.Kind, m.Username)
		}
	}
}
