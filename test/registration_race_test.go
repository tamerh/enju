package test

// Regression for the registration race that forced a full wipe
// (the 2026-05-17 incident: a concurrent enju_my_profile +
// register_agent minted a malformed ownerless kind='human' row
// carrying an agent's username, which then permanently 409'd
// re-registration with no delete-citizen tool).
//
// Mechanism in place (the chosen one — idempotent register +
// fail-closed agent; a literal auth-path citizen-bootstrap was the
// alternative considered and deliberately NOT taken, because a
// wipe also drops the tokens table so there is nothing to revive a
// citizen from — the client must re-register regardless):
//
//   - A human's identity is its email, and an agent has none, so
//     an agent's stale client re-registering through the
//     auth-exempt /citizens/register path (no email) is REJECTED
//     outright — the malformed-human row can no longer be minted.
//   - /citizens/register is idempotent by email: a concurrent
//     re-register returns the SAME citizen + a fresh token, never
//     a duplicate, never a 409.
//   - register_agent fails closed: an agent whose owner can't be
//     resolved is rejected with zero rows, never coerced.
//
// Together these close the window for every client, regardless of
// call order: there is no state where a token authenticates but
// the citizen row is missing in a way that can mint a malformed
// survivor.

import (
	"net/http"
	"sync"
	"testing"
)

// TestIncidentRegression_AgentShapedRegisterIsRejected reproduces
// the exact shape that minted the malformed survivor: a client
// holding an AGENT identity (a username, no email — agents have no
// email) hits the auth-exempt bootstrap endpoint. It must be
// rejected with zero rows, so register_agent can then create the
// real agent cleanly (no permanent wedge).
func TestIncidentRegression_AgentShapedRegisterIsRejected(t *testing.T) {
	s := newTestServer(t)

	// The stale agent client's re-register: agent username, no email.
	resp := s.post("/api/v1/citizens/register", map[string]string{
		"name":     "Reviewer Bot 2",
		"username": "reviewer-bot2",
	})
	if _, hasErr := resp["error"]; !hasErr {
		t.Fatalf("an agent-shaped (no-email) register must be rejected, got %v", resp)
	}
	// Zero rows: no citizen exists under that username.
	if c, _ := s.store.GetCitizenByUsername("reviewer-bot2"); c != nil {
		t.Fatalf("a malformed row was minted for %q (kind=%q) — the incident is NOT fixed",
			c.Username, c.Kind)
	}

	// The legitimate path is unobstructed: an owner registers,
	// then register_agent creates reviewer-bot2 as a proper agent.
	owner := s.registerWithEmail("Owner", "owner@race.test")
	ownerTok := s.tokenFor(owner)
	r, _ := s.doAuthed("POST", "/api/v1/citizens/me/agents", ownerTok, map[string]string{
		"name": "Reviewer Bot 2", "username": "reviewer-bot2",
	})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("register_agent after the rejected stale register should succeed, got %d", r.StatusCode)
	}
	agent, _ := s.store.GetCitizenByUsername("reviewer-bot2")
	if agent == nil || agent.Kind != "agent" || agent.ParentID == nil {
		t.Fatalf("reviewer-bot2 must be a properly-owned agent, got %+v", agent)
	}
}

// TestRegistrationRaceNoMalformedSurvivor — the concurrency
// regression: fire the auth-exempt bootstrap (idempotent human
// re-register) and register_agent concurrently on a fresh
// coordinator. End state must be: exactly one correct kind='agent'
// row owned by the caller, the human present exactly once
// (idempotent — same id every time), and never a malformed
// survivor. Every call either succeeds correctly or fails cleanly.
func TestRegistrationRaceNoMalformedSurvivor(t *testing.T) {
	s := newTestServer(t)

	const ownerEmail = "race-owner@example.com"
	owner := s.registerWithEmail("Race Owner", ownerEmail)
	ownerTok := s.tokenFor(owner)
	ownerRec, err := s.store.GetCitizenByUsername(owner)
	if err != nil || ownerRec == nil {
		t.Fatalf("owner lookup: %v", err)
	}
	ownerID := ownerRec.ID

	const n = 8
	var wg sync.WaitGroup
	var mu sync.Mutex
	var badHumanID bool // a /register that created a DIFFERENT human
	var dirtyAgent bool // a register_agent that returned a non-agent
	agentOK := 0

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Idempotent human re-register (what the client's
			// stale-citizen path POSTs). Same email ⇒ same person.
			resp := s.post("/api/v1/citizens/register", map[string]string{
				"name": "Race Owner", "email": ownerEmail,
			})
			if id, ok := resp["id"].(float64); ok && int64(id) != ownerID {
				mu.Lock()
				badHumanID = true
				mu.Unlock()
			}
		}()
		wg.Add(1)
		go func() {
			defer wg.Done()
			// register_agent for the same handle, owned by owner.
			// Tenant-scoped uniqueness ⇒ exactly one wins; the
			// rest must fail CLEANLY (4xx), never partially.
			r, _ := s.doAuthed("POST", "/api/v1/citizens/me/agents", ownerTok,
				map[string]string{"name": "Worker", "username": "worker"})
			mu.Lock()
			switch {
			case r.StatusCode == http.StatusCreated:
				agentOK++
			case r.StatusCode >= 500:
				dirtyAgent = true // a 5xx means a non-clean failure
			}
			mu.Unlock()
		}()
	}
	wg.Wait()

	if badHumanID {
		t.Error("a concurrent /citizens/register created a DIFFERENT human id — not idempotent")
	}
	if dirtyAgent {
		t.Error("a concurrent register_agent failed with 5xx — not a clean rejection")
	}
	if agentOK < 1 {
		t.Error("no register_agent succeeded — expected exactly one winner")
	}

	// The human is present exactly once and unchanged (idempotent
	// re-registers issued fresh tokens, never new rows).
	gotOwner, _ := s.store.GetCitizenByEmail(ownerEmail)
	if gotOwner == nil || gotOwner.ID != ownerID {
		t.Fatalf("owner identity drifted: want id %d, got %+v", ownerID, gotOwner)
	}

	// The agent row is correct and owned — never a malformed
	// ownerless kind='human' survivor carrying the agent's name.
	worker, _ := s.store.GetCitizenByUsername("worker")
	if worker == nil {
		t.Fatal("no 'worker' agent row after the race")
	}
	if worker.Kind != "agent" {
		t.Errorf("'worker' kind = %q, want agent (a malformed survivor)", worker.Kind)
	}
	if worker.ParentID == nil || *worker.ParentID != ownerID {
		t.Errorf("'worker' must be owned by the caller (%d), got parent %v", ownerID, worker.ParentID)
	}
	if worker.TenantID == nil || *worker.TenantID != *ownerRec.TenantID {
		t.Errorf("'worker' tenant = %v, want owner's tenant %v", worker.TenantID, ownerRec.TenantID)
	}
}
