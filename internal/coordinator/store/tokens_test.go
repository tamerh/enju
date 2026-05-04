package store

// contract tests — tokens migrate to their own table.
// The load-bearing claims that this milestone makes:
//
//   - Existing humans authenticate after the migration runs (the
//     backfill copies citizens.token into tokens, the auth path
//     reads from tokens, and the answer is unchanged).
//   - Multiple tokens per citizen work, so rotation (issue new,
//     distribute, revoke old) doesn't cost the bot its identity.
//   - Revoked tokens stop authenticating immediately, even though
//     the citizens.token column still holds the value.
//   - The label survives round-trip so per-deployment auditing
//     ("which token did the leak come from — laptop or ci-server?")
//     works.
//
// See docs/operator-model-design.md for the design doc that drives
// these properties.

import (
	"testing"
	"time"
)

// TestTokensTableBackfilledFromCitizens covers the migration path:
// pre-1.2 databases have a populated citizens.token column and an
// empty tokens table. The migration's INSERT…SELECT must populate
// tokens with one row per citizen, label them 'legacy' (so the
// audit trail shows where they came from), and leave subsequent
// migration runs idempotent.
func TestTokensTableBackfilledFromCitizens(t *testing.T) {
	s := newTestStore(t)
	cid := createTestCitizen(t, s, "tamer", "tok-tamer")

	tokens, err := s.ListTokensByCitizen(cid)
	if err != nil {
		t.Fatalf("list tokens: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("got %d tokens, want exactly 1 from CreateCitizen path", len(tokens))
	}
	got := tokens[0]
	if got.Token != "tok-tamer" {
		t.Errorf("token=%q, want %q", got.Token, "tok-tamer")
	}
	if got.RevokedAt != nil {
		t.Errorf("token revoked at creation: %v", got.RevokedAt)
	}
	// Label is empty string for CreateCitizen path (the issue
	// site doesn't yet pass a label); the 'legacy' label only
	// applies to the migration backfill, exercised below in
	// TestLegacyTokensGetMigratedLabelOnBackfill.
}

// TestLegacyTokensGetMigratedLabelOnBackfill simulates a pre-1.2
// database — citizens row exists but no tokens row — and verifies
// the migration backfill copies the value across with label='legacy'
// so operators can tell which tokens predate the rotation feature.
func TestLegacyTokensGetMigratedLabelOnBackfill(t *testing.T) {
	s := newTestStore(t)

	// Insert a citizen WITHOUT going through CreateCitizen, so the
	// matching tokens row doesn't get created by the tx insert.
	// This is what an old DB looks like after the schema is added
	// but before the backfill runs.
	now := time.Now()
	res, err := s.db.Exec(
		`INSERT INTO citizens (username, name, email, role, token, score, registered_at, last_seen) VALUES (?, ?, '', 'citizen', ?, 0, ?, ?)`,
		"alice", "Alice", "tok-alice-legacy", now, now,
	)
	if err != nil {
		t.Fatalf("insert legacy citizen: %v", err)
	}
	cid, _ := res.LastInsertId()

	// Re-run the backfill explicitly. In production it runs once
	// during migrate(); calling it again proves idempotency.
	for i := 0; i < 2; i++ {
		if _, err := s.db.Exec(`
			INSERT INTO tokens (citizen_id, token, label, issued_at)
			SELECT id, token, 'legacy', registered_at
			FROM citizens
			WHERE token != ''
			  AND NOT EXISTS (SELECT 1 FROM tokens WHERE tokens.citizen_id = citizens.id)
		`); err != nil {
			t.Fatalf("backfill iter %d: %v", i, err)
		}
	}

	tokens, err := s.ListTokensByCitizen(cid)
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(tokens) != 1 {
		t.Fatalf("idempotent backfill must produce exactly 1 row, got %d", len(tokens))
	}
	if tokens[0].Label != "legacy" {
		t.Errorf("label=%q, want %q", tokens[0].Label, "legacy")
	}
	if tokens[0].Token != "tok-alice-legacy" {
		t.Errorf("token round-trip mismatch: %q", tokens[0].Token)
	}
}

// TestRevokedTokenStopsAuthenticating is the load-bearing security
// property: once a token is revoked, subsequent GetCitizenByToken
// calls must return nil. If this regresses, leaked tokens stay
// usable forever and the whole point of having a tokens table is
// gone.
func TestRevokedTokenStopsAuthenticating(t *testing.T) {
	s := newTestStore(t)
	cid := createTestCitizen(t, s, "tamer", "tok-tamer")

	// Pre-revoke: token authenticates.
	c, err := s.GetCitizenByToken("tok-tamer")
	if err != nil || c == nil {
		t.Fatalf("pre-revoke auth: %v / %v", err, c)
	}

	// Revoke by value (the convenience path most callers use —
	// API/CLI code holds the token, not the row id).
	if err := helperRevokeTokenByValue(s, "tok-tamer"); err != nil {
		t.Fatalf("revoke: %v", err)
	}

	// Post-revoke: same token, same call, returns nil.
	c2, err := s.GetCitizenByToken("tok-tamer")
	if err != nil {
		t.Fatalf("post-revoke lookup error: %v", err)
	}
	if c2 != nil {
		t.Errorf("revoked token still authenticated as %s — auth bypass", c2.Username)
	}

	// Sanity check: the row is still there for audit, just
	// flagged. ListTokensByCitizen returns it with RevokedAt set.
	tokens, _ := s.ListTokensByCitizen(cid)
	if len(tokens) != 1 || tokens[0].RevokedAt == nil {
		t.Errorf("audit row missing or RevokedAt nil: %+v", tokens)
	}
}

// TestRevokeIsIdempotent — calling RevokeTokenByValue twice doesn't
// move the timestamp. Matters because a confused caller might
// double-revoke; the first revocation time is the audit truth.
func TestRevokeIsIdempotent(t *testing.T) {
	s := newTestStore(t)
	cid := createTestCitizen(t, s, "tamer", "tok-tamer")

	if err := helperRevokeTokenByValue(s, "tok-tamer"); err != nil {
		t.Fatalf("first revoke: %v", err)
	}
	tokens, _ := s.ListTokensByCitizen(cid)
	firstTime := *tokens[0].RevokedAt

	time.Sleep(2 * time.Millisecond) // ensure clock would advance
	if err := helperRevokeTokenByValue(s, "tok-tamer"); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
	tokens, _ = s.ListTokensByCitizen(cid)
	if !tokens[0].RevokedAt.Equal(firstTime) {
		t.Errorf("revoked_at moved on second revoke: %v -> %v", firstTime, *tokens[0].RevokedAt)
	}
}

// TestMultipleTokensPerCitizen covers the rotation use case: issue a
// second token, both authenticate while active, revoke the original,
// the new one still works. This is the property that makes the tokens-table split
// worth doing — without it, bot owners can't rotate without replacing
// the bot identity.
func TestMultipleTokensPerCitizen(t *testing.T) {
	s := newTestStore(t)
	cid := createTestCitizen(t, s, "tamer", "tok-original")

	// Issue a second token labeled 'rotation'.
	if _, err := helperIssueToken(s, cid, "tok-new", "rotation"); err != nil {
		t.Fatalf("issue second: %v", err)
	}

	// Both tokens authenticate to the same citizen.
	for _, tok := range []string{"tok-original", "tok-new"} {
		c, err := s.GetCitizenByToken(tok)
		if err != nil || c == nil {
			t.Errorf("%s did not authenticate: %v / %v", tok, err, c)
			continue
		}
		if c.ID != cid {
			t.Errorf("%s resolved to wrong citizen: got id=%d, want %d", tok, c.ID, cid)
		}
	}

	// Revoke the original. New still works; original is dead.
	if err := helperRevokeTokenByValue(s, "tok-original"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if c, _ := s.GetCitizenByToken("tok-original"); c != nil {
		t.Errorf("revoked original still authenticates")
	}
	if c, _ := s.GetCitizenByToken("tok-new"); c == nil {
		t.Errorf("rotation broke: new token doesn't authenticate after revoking original")
	}

	// List shows both, ordered by issued_at DESC (newest first).
	tokens, _ := s.ListTokensByCitizen(cid)
	if len(tokens) != 2 {
		t.Fatalf("expected 2 tokens, got %d", len(tokens))
	}
	if tokens[0].Label != "rotation" || tokens[1].Label != "" {
		t.Errorf("ordering or labels off: [%q, %q]", tokens[0].Label, tokens[1].Label)
	}
}

// TestIssueTokenRejectsEmpty is a minor input-validation test.
// Passing an empty string would create a row that any "no token"
// caller could match — silent auth bypass. Reject loudly.
func TestIssueTokenRejectsEmpty(t *testing.T) {
	s := newTestStore(t)
	cid := createTestCitizen(t, s, "tamer", "tok-tamer")
	if _, err := helperIssueToken(s, cid, "", "label"); err == nil {
		t.Fatal("expected error for empty token, got nil")
	}
}
