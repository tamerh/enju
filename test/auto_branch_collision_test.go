package test

// Regression tests for the on-disk vs coord-DB divergence bug:
//
//   coord DB and the bare git repo are two stores of truth. After
//   a coord DB wipe, the on-disk repo still carries every branch
//   from prior sessions. enju_create_run with branch="auto" picks
//   a name based on what's actually on disk (fat-client side) —
//   the coord no longer allocates auto names. If a future caller
//   bypasses the fat-client and hits coord directly with
//   branch="auto", coord must error loudly rather than allocating
//   from a DB-only view that's known to drift.
//
// The architectural split this test pins:
//   - coord owns DAG + state + events; does NOT touch git
//   - fat-client owns git operations including branch allocation
//   - branch="auto" is a fat-client-side convenience resolved
//     against the bare repo before the create_run request reaches
//     coord
//
// Two distinct assertions, two distinct tests.

import (
	"fmt"
	"os"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestFatClientAutoBranchAvoidsOnDiskCollisions is the load-
// bearing test: MCP client passes branch="auto", and the fat-
// client resolves it against the bare repo BEFORE sending to
// coord. A branch that exists on disk but isn't in the coord DB
// (post-wipe shape) must NOT be re-allocated.
func TestFatClientAutoBranchAvoidsOnDiskCollisions(t *testing.T) {
	h := newMCPHarness(t, "Auto Branch Alice")
	projectID := h.createTestProject()

	// Pre-seed an on-disk branch the coord DB has no knowledge
	// of. Mirrors what survives a coord DB wipe: the bare repo
	// still carries every branch from prior sessions.
	//
	// Slug for "Simple No Dependencies" (the YAML name field
	// in testdata/simple-no-deps.yaml) is
	// "simple-no-dependencies". The naive picker would try
	// simple-no-dependencies-1 first.
	barePath := h.remotes[projectID]
	if barePath == "" {
		t.Fatalf("test setup: remote path not cached")
	}
	seedOrphanBareBranch(t, barePath, "simple-no-dependencies-1")

	// Drive create_run via MCP with branch="auto". The fat-
	// client intercept should resolve to a name that avoids
	// the orphan on disk.
	yamlData, _ := os.ReadFile("testdata/simple-no-deps.yaml")
	res := h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       string(yamlData),
		"branch":     "auto",
	})
	text := mcpText(res)
	// The MCP response format includes the assigned branch
	// name; grep for it. Format.CreateRun prints
	// "Branch: <name>" among other fields.
	if strings.Contains(text, "Branch: simple-no-dependencies-1") {
		t.Errorf("fat-client picked simple-no-dependencies-1, colliding "+
			"with on-disk branch. The fat-client's auto-branch resolver "+
			"didn't consult the bare repo. Output:\n%s", text)
	}
	if !strings.Contains(text, "simple-no-dependencies-2") {
		t.Logf("did not see simple-no-dependencies-2 in output (could "+
			"have picked -3 or later if more on-disk collisions existed); "+
			"output:\n%s", text)
	}
}

// TestCoordRejectsAutoBranchDirectly pins the architectural
// boundary: when coord receives branch="auto" directly (bypassing
// the fat-client intercept — e.g., curl-direct callers), it must
// reject with a clear message explaining that auto resolution is
// a client-side convenience.
//
// Pre-fix, coord had its own DB-only "auto" picker that produced
// names which collided with on-disk branches the DB had no record
// of. Post-fix, coord delegates the responsibility to whoever has
// access to the bare repo (the fat-client). This test guards
// against accidentally re-adding the coord-side picker.
func TestCoordRejectsAutoBranchDirectly(t *testing.T) {
	s := newTestServer(t)
	projectID := s.createTestProject()

	yamlData, _ := os.ReadFile("testdata/simple-no-deps.yaml")
	resp := s.post(fmt.Sprintf("/api/v1/projects/%d/runs", projectID), map[string]string{
		"yaml":   string(yamlData),
		"branch": "auto",
	})

	errMsg, _ := resp["error"].(string)
	if errMsg == "" {
		t.Fatalf("coord accepted branch=auto from direct REST caller — must reject "+
			"so the picker behavior stays in one place (fat-client). Response: %+v", resp)
	}
	if !strings.Contains(errMsg, "auto") || !strings.Contains(errMsg, "client") {
		t.Errorf("error message should mention auto is a client-side convenience; "+
			"got: %q", errMsg)
	}
}

// seedOrphanBareBranch adds a branch ref to the bare repo that
// the coord DB doesn't know about. Mirrors what a prior session
// would have left on disk after a DB wipe. Uses the existing
// `main` commit as the target so we don't need to push new
// objects — the picker collision is purely about the ref NAME.
func seedOrphanBareBranch(t *testing.T, barePath, branchName string) {
	t.Helper()
	bare, err := gogit.PlainOpen(barePath)
	if err != nil {
		t.Fatalf("open bare %s: %v", barePath, err)
	}
	mainRef, err := bare.Reference(plumbing.NewBranchReferenceName("main"), false)
	if err != nil {
		t.Fatalf("resolving main on bare: %v", err)
	}
	branchRef := plumbing.NewBranchReferenceName(branchName)
	if err := bare.Storer.SetReference(plumbing.NewHashReference(branchRef, mainRef.Hash())); err != nil {
		t.Fatalf("set ref %s: %v", branchRef, err)
	}
}
