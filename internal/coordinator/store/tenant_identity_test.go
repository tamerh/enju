package store

// Contract tests for the identity model: tenant scoping, per-kind
// uniqueness, and fail-closed agent registration. Pinned at the
// apply layer where the behavior is deterministic (no HTTP, no
// client call-order).

import (
	"strings"
	"testing"
	"time"
)

func mkRoot(t *testing.T, s *Store, username, email string) int64 {
	t.Helper()
	now := time.Now()
	id, err := helperCreateCitizen(s, &CitizenRecord{
		Username: username, Name: username, Email: email,
		RegisteredAt: now, LastSeen: now,
	}, "tok-"+username)
	if err != nil {
		t.Fatalf("create root %q: %v", username, err)
	}
	return id
}

func mkAgent(t *testing.T, s *Store, username string, ownerID int64) (int64, error) {
	now := time.Now()
	return helperCreateCitizen(s, &CitizenRecord{
		Username: username, Name: username,
		Kind: CitizenKindBot, ParentID: &ownerID,
		RegisteredAt: now, LastSeen: now,
	}, "tok-"+username+"-"+time.Now().Format("150405.000000"))
}

// TestHumanRootTenantIsSelf — a human root (no parent) is its own
// tenant. Single-operator self-host is exactly this one-tenant
// case, which is why per-tenant uniqueness ≡ old global behavior.
func TestHumanRootTenantIsSelf(t *testing.T) {
	s := newTestStore(t)
	id := mkRoot(t, s, "tamer", "tamer@example.com")

	c, err := s.GetCitizen(id)
	if err != nil || c == nil {
		t.Fatalf("get root: %v", err)
	}
	if c.TenantID == nil || *c.TenantID != id {
		t.Errorf("root tenant_id = %v, want self (%d)", c.TenantID, id)
	}
}

// TestAgentInheritsOwnerTenant — an agent's tenant is its owner's
// tenant (the root of the parent chain), not its own id.
func TestAgentInheritsOwnerTenant(t *testing.T) {
	s := newTestStore(t)
	rootID := mkRoot(t, s, "tamer", "tamer@example.com")
	agentID, err := mkAgent(t, s, "dev-bot", rootID)
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}
	a, _ := s.GetCitizen(agentID)
	if a == nil || a.TenantID == nil || *a.TenantID != rootID {
		t.Errorf("agent tenant_id = %v, want owner's tenant (%d)", a.TenantID, rootID)
	}
}

// TestTwoTenantsEachRegisterSameAgentHandle — the multi-tenant
// property: two tenant roots each register "dev-bot"
// — BOTH succeed (no collision), and the handle resolves to the
// correct agent within each tenant.
func TestTwoTenantsEachRegisterSameAgentHandle(t *testing.T) {
	s := newTestStore(t)

	rootA := mkRoot(t, s, "owner-a", "a@example.com")
	rootB := mkRoot(t, s, "owner-b", "b@example.com")

	agentA, err := mkAgent(t, s, "dev-bot", rootA)
	if err != nil {
		t.Fatalf("tenant A dev-bot should register: %v", err)
	}
	agentB, err := mkAgent(t, s, "dev-bot", rootB)
	if err != nil {
		t.Fatalf("tenant B dev-bot should register (no global collision): %v", err)
	}
	if agentA == agentB {
		t.Fatal("the two dev-bots must be distinct citizens")
	}

	// The handle resolves per-tenant: "dev-bot" in tenant A is
	// agentA, in tenant B is agentB. Prefix-free — the same bare
	// string, disambiguated only by tenant.
	gotA, _ := s.GetCitizenByUsernameInTenant("dev-bot", rootA)
	gotB, _ := s.GetCitizenByUsernameInTenant("dev-bot", rootB)
	if gotA == nil || gotA.ID != agentA {
		t.Errorf("dev-bot in tenant A = %v, want %d", gotA, agentA)
	}
	if gotB == nil || gotB.ID != agentB {
		t.Errorf("dev-bot in tenant B = %v, want %d", gotB, agentB)
	}
}

// TestSameTenantDuplicateAgentHandleRejected — within ONE tenant a
// handle is still unique (an owner can't have two "dev-bot"s).
func TestSameTenantDuplicateAgentHandleRejected(t *testing.T) {
	s := newTestStore(t)
	rootID := mkRoot(t, s, "tamer", "tamer@example.com")

	if _, err := mkAgent(t, s, "dev-bot", rootID); err != nil {
		t.Fatalf("first dev-bot: %v", err)
	}
	_, err := mkAgent(t, s, "dev-bot", rootID)
	if err == nil {
		t.Fatal("a second dev-bot under the same owner must be rejected")
	}
	if !strings.Contains(err.Error(), "already taken") {
		t.Errorf("error should explain the in-tenant collision; got: %v", err)
	}
}

// TestRegisterAgentFailsClosedOnUnresolvedOwner — an agent whose
// owner can't be resolved is rejected with ZERO rows written,
// never coerced into some other shape.
func TestRegisterAgentFailsClosedOnUnresolvedOwner(t *testing.T) {
	s := newTestStore(t)

	var before int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM citizens`).Scan(&before)

	ghost := int64(999999) // no such citizen
	_, err := mkAgent(t, s, "orphan-bot", ghost)
	if err == nil {
		t.Fatal("registering an agent with an unresolved owner must fail")
	}
	if !strings.Contains(err.Error(), "ownerless") &&
		!strings.Contains(err.Error(), "owner citizen") {
		t.Errorf("error should name the fail-closed reason; got: %v", err)
	}

	var after int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM citizens`).Scan(&after)
	if after != before {
		t.Errorf("fail-closed must write ZERO rows: citizen count %d → %d", before, after)
	}
}

// TestHumanUsernameGloballyUnique — a person's handle is globally
// unique (policy on top of email-as-identity: no two humans share
// a username). Distinct from agents, which stay tenant-scoped
// (TestTwoTenantsEachRegisterSameAgentHandle proves two owners can
// each have a "dev-bot"). This is the asymmetry: humans global,
// agents per-tenant.
func TestHumanUsernameGloballyUnique(t *testing.T) {
	s := newTestStore(t)
	mkRoot(t, s, "tamer", "tamer-1@example.com")

	now := time.Now()
	_, err := helperCreateCitizen(s, &CitizenRecord{
		Username: "tamer", Name: "Other Tamer", Email: "tamer-2@example.com",
		RegisteredAt: now, LastSeen: now,
	}, "tok-tamer-2")
	if err == nil {
		t.Fatal("a second human reusing an existing handle must be rejected")
	}
	if !strings.Contains(err.Error(), "already taken") {
		t.Errorf("error should explain the handle collision; got: %v", err)
	}
}

// TestEmailMandatoryForHuman — a human's global identity is its
// email; creating one without an email is rejected.
func TestEmailMandatoryForHuman(t *testing.T) {
	s := newTestStore(t)
	now := time.Now()
	_, err := helperCreateCitizen(s, &CitizenRecord{
		Username: "no-email", Name: "No Email",
		RegisteredAt: now, LastSeen: now,
	}, "tok-no-email")
	if err == nil {
		t.Fatal("a human with no email must be rejected")
	}
	if !strings.Contains(err.Error(), "email is required") {
		t.Errorf("error should explain email is required; got: %v", err)
	}
}

// TestHumanEmailGloballyUnique — email is the human identity, so a
// second human with the same email is rejected even across tenants
// (each human root is its own tenant; the username could differ
// but the email cannot).
func TestHumanEmailGloballyUnique(t *testing.T) {
	s := newTestStore(t)
	mkRoot(t, s, "alice", "shared@example.com")

	now := time.Now()
	_, err := helperCreateCitizen(s, &CitizenRecord{
		Username: "bob", Name: "Bob", Email: "shared@example.com",
		RegisteredAt: now, LastSeen: now,
	}, "tok-bob")
	if err == nil {
		t.Fatal("a second human with an existing email must be rejected")
	}
	if !strings.Contains(err.Error(), "email already exists") {
		t.Errorf("error should explain the email collision; got: %v", err)
	}
}
