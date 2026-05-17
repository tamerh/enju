package store

// Contract tests for the cosmetic model_catalog.
//
// A model is NOT a citizen — it has no row in citizens, no token,
// no lifecycle. model_catalog is a display-name lookup only and is
// explicitly NOT correctness-bearing: a model name with no catalog
// entry just renders raw. The properties pinned here:
//
//   - The hand-curated seed lands automatically on first migration
//     and is idempotent on re-run (a coordinator restart doesn't
//     duplicate or perturb the catalog).
//   - ModelDisplayName returns the pretty string for a seeded name,
//     and falls back to the RAW name for unknown / empty input —
//     never an error, never a citizen lookup.
//   - No seeded model name is ever a citizen (the cleanup guard:
//     models stopped pretending to be citizens).

import "testing"

// TestSeedCatalogPopulatedOnFirstMigration verifies the migration
// inserted every modelCatalogSeed row into model_catalog with its
// display name. Drift between the seed and the table would silently
// degrade dashboard rendering.
func TestSeedCatalogPopulatedOnFirstMigration(t *testing.T) {
	s := newTestStore(t)

	for _, m := range modelCatalogSeed {
		if got := s.ModelDisplayName(m.Name); got != m.DisplayName {
			t.Errorf("ModelDisplayName(%q) = %q, want %q", m.Name, got, m.DisplayName)
		}
	}
}

// TestSeedIsIdempotent — re-running the seed (a coordinator
// restart on the same DB) must not crash, duplicate, or modify
// catalog rows. INSERT OR IGNORE pins this.
func TestSeedIsIdempotent(t *testing.T) {
	s := newTestStore(t)

	if err := s.seedModelCatalog(); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	for _, m := range modelCatalogSeed {
		var count int
		if err := s.db.QueryRow(
			`SELECT COUNT(*) FROM model_catalog WHERE name = ?`, m.Name,
		).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count != 1 {
			t.Errorf("%s appears %d times after re-seed; want exactly 1", m.Name, count)
		}
	}
}

// TestModelDisplayNameFallsBackToRaw — an unknown model name (a
// local Ollama finetune the seed doesn't cover) and the empty
// string must round-trip unchanged. The catalog is cosmetic: a
// miss is never an error.
func TestModelDisplayNameFallsBackToRaw(t *testing.T) {
	s := newTestStore(t)

	if got := s.ModelDisplayName("ollama-some-local-finetune"); got != "ollama-some-local-finetune" {
		t.Errorf("unknown name = %q, want the raw name back", got)
	}
	if got := s.ModelDisplayName(""); got != "" {
		t.Errorf("empty name = %q, want \"\"", got)
	}
}

// TestSeededModelNamesAreNotCitizens is the cleanup guard: after
// the identity change a model is a label on the work, never a
// citizen. No seeded model name may resolve to a citizen row, and
// no kind='model' row may exist.
func TestSeededModelNamesAreNotCitizens(t *testing.T) {
	s := newTestStore(t)

	for _, m := range modelCatalogSeed {
		c, err := s.GetCitizenByUsername(m.Name)
		if err != nil {
			t.Fatalf("lookup %s: %v", m.Name, err)
		}
		if c != nil {
			t.Errorf("model %q resolved to a citizen (id=%d, kind=%q) — a model must not be a citizen",
				m.Name, c.ID, c.Kind)
		}
	}

	var modelKindRows int
	if err := s.db.QueryRow(
		`SELECT COUNT(*) FROM citizens WHERE kind = 'model'`,
	).Scan(&modelKindRows); err != nil {
		t.Fatal(err)
	}
	if modelKindRows != 0 {
		t.Errorf("found %d kind='model' citizen rows; want 0 (the kind no longer exists)", modelKindRows)
	}
}
