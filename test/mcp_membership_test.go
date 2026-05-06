package test

// Project membership (Phase J) integration tests.
//
// The MCP harness creates projects via s.post without an auth
// header, so they land as legacy "zero-members" projects that
// bypass gating. These tests go through enju_create_project on an
// authenticated TestClient instead, so creator auto-add happens
// and gating is live. That split is intentional: existing tests
// stay on the soft-auth legacy path; membership tests opt into
// the gated path by creating projects via the MCP client.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/fatclient/mcphandlers"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// mcpCreateProjectAs fires enju_create_project through the given
// TestClient and returns the new project's ID. The calling client
// is auto-added as owner by the coordinator.
func mcpCreateProjectAs(t *testing.T, h *mcpHarness, client *mcphandlers.TestClient, name string) int64 {
	t.Helper()
	res, err := client.Call(context.Background(), "enju_create_project", map[string]any{
		"name": name,
		"path": t.TempDir(),
	})
	if err != nil {
		t.Fatalf("create_project: %v", err)
	}
	if res.IsError {
		t.Fatalf("create_project returned error: %s", mcpText(res))
	}
	proj, err := h.store.GetProjectByName(name)
	if err != nil || proj == nil {
		t.Fatalf("project %q not found after create: %v", name, err)
	}
	return proj.ID
}

// TestMCPMembershipCreatorAutoAdd verifies a project created via
// enju_create_project seeds the caller as owner.
func TestMCPMembershipCreatorAutoAdd(t *testing.T) {
	eachRemoteMode(t, "Creator Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-auto-add-%d", nowNano()))

		members, err := h.store.ListProjectMembers(projectID)
		if err != nil {
			t.Fatalf("list members: %v", err)
		}
		if len(members) != 1 {
			t.Fatalf("expected 1 member after create, got %d: %+v", len(members), members)
		}
		if members[0].Role != store.ProjectRoleOwner {
			t.Fatalf("expected creator to be owner, got %q", members[0].Role)
		}
		creatorID := h.citizenID(h.username)
		if members[0].CitizenID != creatorID {
			t.Fatalf("expected creator citizen_id %d, got %d", creatorID, members[0].CitizenID)
		}
	})
}

// TestMCPMembershipNonMemberBlocked verifies a non-member gets a
// 403 on project-scoped reads (enju_list_project_members) and
// that creator can still see the project list.
func TestMCPMembershipNonMemberBlocked(t *testing.T) {
	eachRemoteMode(t, "Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-gate-%d", nowNano()))

		stranger := h.newMCPClientAs(t, "Stranger")

		// Stranger tries to list members — must 403.
		res, err := stranger.Call(context.Background(), "enju_list_project_members", map[string]any{
			"project_id": float64(projectID),
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected stranger to be blocked, got success: %s", mcpText(res))
		}
		if !strings.Contains(mcpText(res), "not a member") {
			t.Errorf("expected 'not a member' in error, got: %s", mcpText(res))
		}

		// Owner sees the member list fine and themselves listed.
		ownerRes := h.callOK(t, "enju_list_project_members", map[string]any{"project_id": float64(projectID)})
		if !strings.Contains(mcpText(ownerRes), "@"+h.username) {
			t.Errorf("expected owner listing to mention @%s, got: %s", h.username, mcpText(ownerRes))
		}
	})
}

// TestMCPMembershipMemberCanAdd verifies any member can invite
// another citizen — the trust-based delegation path the user
// asked for.
func TestMCPMembershipMemberCanAdd(t *testing.T) {
	eachRemoteMode(t, "Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-add-%d", nowNano()))

		// Seed a new citizen (bob) and have the owner invite them.
		bobName := "Bob " + fmt.Sprintf("%d", nowNano())
		bobUsername := h.register(bobName)
		h.callOK(t, "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   bobUsername,
		})

		// Now bob invites a third citizen — verifies member-can-add.
		bobClient := newTestClientFor(t, h, bobUsername, bobName)
		carolName := "Carol " + fmt.Sprintf("%d", nowNano())
		carolUsername := h.register(carolName)
		res, err := bobClient.Call(context.Background(), "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   carolUsername,
		})
		if err != nil {
			t.Fatalf("bob adds carol: %v", err)
		}
		if res.IsError {
			t.Fatalf("bob-add-carol: %s", mcpText(res))
		}

		members, _ := h.store.ListProjectMembers(projectID)
		if len(members) != 3 {
			t.Fatalf("expected 3 members after bob invites carol, got %d", len(members))
		}
	})
}

// TestMCPMembershipMemberCannotAddAsOwner verifies a non-owner
// can't slip a new member in with role='owner'.
func TestMCPMembershipMemberCannotAddAsOwner(t *testing.T) {
	eachRemoteMode(t, "Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-add-owner-%d", nowNano()))

		bobName := "Bob " + fmt.Sprintf("%d", nowNano())
		bobUsername := h.register(bobName)
		h.callOK(t, "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   bobUsername,
		})

		bobClient := newTestClientFor(t, h, bobUsername, bobName)
		carolUsername := h.register("Carol " + fmt.Sprintf("%d", nowNano()))
		res, err := bobClient.Call(context.Background(), "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   carolUsername,
			"role":       "owner",
		})
		if err != nil {
			t.Fatalf("call: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected member-adding-as-owner to be refused, got: %s", mcpText(res))
		}
		if !strings.Contains(mcpText(res), "only project owners") {
			t.Errorf("expected 'only project owners' error, got: %s", mcpText(res))
		}
	})
}

// TestMCPMembershipPromoteDemote verifies an owner can promote a
// member and later demote that owner back to member — plus the
// last-owner demote refusal.
func TestMCPMembershipPromoteDemote(t *testing.T) {
	eachRemoteMode(t, "Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-promote-%d", nowNano()))

		bobUsername := h.register("Bob " + fmt.Sprintf("%d", nowNano()))
		h.callOK(t, "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   bobUsername,
		})

		// Owner demoting themselves while they're the only owner is refused.
		res := h.callExpectError(t, "enju_demote_owner", map[string]any{
			"project_id": float64(projectID),
			"username":   h.username,
		})
		if !strings.Contains(res, "last owner") {
			t.Errorf("expected last-owner error, got: %s", res)
		}

		// Promote bob → owner.
		h.callOK(t, "enju_promote_member", map[string]any{
			"project_id": float64(projectID),
			"username":   bobUsername,
		})
		bobID := h.citizenID(bobUsername)
		bob, _ := h.store.GetProjectMember(projectID, bobID)
		if bob == nil || bob.Role != store.ProjectRoleOwner {
			t.Fatalf("expected bob to be owner after promote, got %+v", bob)
		}

		// Now demoting the original owner is fine (bob is still owner).
		h.callOK(t, "enju_demote_owner", map[string]any{
			"project_id": float64(projectID),
			"username":   h.username,
		})
		ownerID := h.citizenID(h.username)
		self, _ := h.store.GetProjectMember(projectID, ownerID)
		if self == nil || self.Role != store.ProjectRoleMember {
			t.Fatalf("expected original owner to be demoted to member, got %+v", self)
		}
	})
}

// TestMCPMembershipLeaveLastOwnerRefused verifies self-leave is
// refused when the caller is the only remaining owner, preserving
// the ≥1-owner invariant.
func TestMCPMembershipLeaveLastOwnerRefused(t *testing.T) {
	eachRemoteMode(t, "Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-leave-last-%d", nowNano()))

		res := h.callExpectError(t, "enju_leave_project", map[string]any{
			"project_id": float64(projectID),
		})
		if !strings.Contains(res, "last owner") {
			t.Errorf("expected last-owner error on leave, got: %s", res)
		}
		// Owner is still a member — no partial removal.
		ownerID := h.citizenID(h.username)
		m, _ := h.store.GetProjectMember(projectID, ownerID)
		if m == nil {
			t.Fatalf("owner unexpectedly removed after refused leave")
		}
	})
}

// TestMCPMembershipOwnerRemovesMember verifies an owner can kick
// a regular member but a regular member cannot kick another.
func TestMCPMembershipOwnerRemovesMember(t *testing.T) {
	eachRemoteMode(t, "Owner", func(t *testing.T, h *mcpHarness) {
		projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("membership-remove-%d", nowNano()))

		bobName := "Bob " + fmt.Sprintf("%d", nowNano())
		bobUsername := h.register(bobName)
		h.callOK(t, "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   bobUsername,
		})
		carolName := "Carol " + fmt.Sprintf("%d", nowNano())
		carolUsername := h.register(carolName)
		h.callOK(t, "enju_add_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   carolUsername,
		})

		// Bob (member) tries to remove carol — refused.
		bobClient := newTestClientFor(t, h, bobUsername, bobName)
		res, err := bobClient.Call(context.Background(), "enju_remove_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   carolUsername,
		})
		if err != nil {
			t.Fatalf("bob remove carol: %v", err)
		}
		if !res.IsError {
			t.Fatalf("expected member-removes-member to be refused, got: %s", mcpText(res))
		}

		// Owner removes bob — succeeds.
		h.callOK(t, "enju_remove_project_member", map[string]any{
			"project_id": float64(projectID),
			"username":   bobUsername,
		})
		bobID := h.citizenID(bobUsername)
		m, _ := h.store.GetProjectMember(projectID, bobID)
		if m != nil {
			t.Fatalf("expected bob removed, still present: %+v", m)
		}
	})
}

// newTestClientFor wires a second TestClient as a specific
// pre-registered citizen. Mirrors newMCPClientAs but takes an
// already-known username rather than registering a new one.
func newTestClientFor(t *testing.T, h *mcpHarness, username, displayName string) *mcphandlers.TestClient {
	t.Helper()
	cz, err := h.store.GetCitizenByUsername(username)
	if err != nil || cz == nil {
		t.Fatalf("lookup %s: %v", username, err)
	}
	cfg := mcphandlers.Config{
		CoordinatorURL: h.url,
		Username:       username,
		CitizenName:    displayName,
		CitizenEmail:   cz.Email,
		AuthToken:      cz.Token,
		ModelName:      "test-model",
		Workspace:      h.project,
		Logger:         slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	return mcphandlers.NewTestClient(cfg)
}

// nowNano returns a monotonic-ish suffix for unique names.
// Kept local to this file so the tests don't fight the global
// time package.
func nowNano() int64 {
	return nowNanoSeq()
}

// nowNanoSeq bumps a package-local counter each call so two
// tests running in the same nanosecond still get distinct
// project names.
var nowSeqCounter int64

func nowNanoSeq() int64 {
	nowSeqCounter++
	return nowSeqCounter
}
