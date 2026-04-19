package test

// Branch-per-run model (Phase K) integration tests.
//
// The serial-per-branch invariant + branch-aware git writes +
// branch-scoped artifact index are new in Phase K. These tests
// pin the guarantees worth defending:
//
//   - Two active runs on the same branch → second one refused
//   - branch="auto" picks an unused run-N
//   - Each run's commits land on its own branch
//   - Artifact index rows are keyed by (project, branch, path)
//   - enju_set_project_default_branch flips the default for new
//     runs (owner-only)

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	gogit "github.com/go-git/go-git/v5"
	"github.com/go-git/go-git/v5/plumbing"
)

// TestMCPBranchSerialRunsRefused verifies the coordinator
// refuses a second active run on the same branch with a clear
// error pointing at the existing run.
func TestMCPBranchSerialRunsRefused(t *testing.T) {
	h := newMCPHarness(t, "SerialBranch")
	projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("branch-serial-%d", nowNano()))

	yaml := `name: "first run"
version: 1
tasks:
  - id: only
    action: answer
    prompt: "Hi."
`
	res1, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
	})
	if err != nil {
		t.Fatalf("create run 1: %v", err)
	}
	if res1.IsError {
		t.Fatalf("create run 1 rejected: %s", mcpText(res1))
	}

	// Second run on the same (default) branch while #1 is active
	// — must be refused. handleCreateRun wraps the coordinator's
	// 409 in a ✗ success result (formatter pattern), so the
	// IsError bit stays false; assertion is on the text.
	res2, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
	})
	if err != nil {
		t.Fatalf("create run 2: %v", err)
	}
	msg := mcpText(res2)
	if !strings.Contains(msg, "already has an active run") {
		t.Errorf("expected serial-run error, got: %s", msg)
	}
	if !strings.Contains(msg, `"auto"`) {
		t.Errorf("expected error to suggest branch=\"auto\"; got: %s", msg)
	}
	// Second run must NOT have landed on the store side.
	runs, _ := h.store.ListRunsByProject(projectID)
	if len(runs) != 1 {
		t.Fatalf("expected serial-runs refusal to prevent run creation, got %d runs", len(runs))
	}
}

// TestMCPBranchAutoAllocation verifies branch="auto" picks an
// unused run-N and successive auto calls walk run-2, run-3, ...
func TestMCPBranchAutoAllocation(t *testing.T) {
	h := newMCPHarness(t, "AutoBranch")
	projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("branch-auto-%d", nowNano()))

	yaml := `name: "auto-named"
version: 1
tasks:
  - id: only
    action: answer
    prompt: "Hi."
`
	// First run uses default branch (main).
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
	})

	// branch="auto" — picks the first unused run-N slot. Assert
	// on the persisted run record since the tool text doesn't
	// currently surface the chosen branch name.
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "auto",
	})
	// Second auto call → next unused slot.
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "auto",
	})

	runs, err := h.store.ListRunsByProject(projectID)
	if err != nil || len(runs) != 3 {
		t.Fatalf("expected 3 runs, got %d (err=%v)", len(runs), err)
	}
	seen := map[string]bool{}
	for _, r := range runs {
		seen[r.Branch] = true
	}
	if !seen["main"] {
		t.Errorf("expected main among branches; got: %+v", seen)
	}
	// Auto runs pick run-1, run-2 (or run-2, run-3 depending
	// on whether the first run occupied "run-1"). Just assert
	// that the two auto calls produced distinct, run-N-shaped
	// branches.
	autoCount := 0
	for b := range seen {
		if strings.HasPrefix(b, "run-") {
			autoCount++
		}
	}
	if autoCount != 2 {
		t.Errorf("expected 2 run-N branches from auto allocation; got seen=%+v", seen)
	}
}

// TestMCPBranchExplicitName verifies an explicit branch= name
// is accepted, persisted on the run, and shows up in the
// response.
func TestMCPBranchExplicitName(t *testing.T) {
	h := newMCPHarness(t, "ExplicitBranch")
	projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("branch-explicit-%d", nowNano()))

	yaml := `name: "experiment"
version: 1
tasks:
  - id: only
    action: answer
    prompt: "Hi."
`
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "ablation-no-dropout",
	})

	// Look up the run on the store side to confirm persistence.
	runs, err := h.store.ListRunsByProject(projectID)
	if err != nil || len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d (err=%v)", len(runs), err)
	}
	if runs[0].Branch != "ablation-no-dropout" {
		t.Errorf("expected branch=\"ablation-no-dropout\" persisted on run, got %q", runs[0].Branch)
	}
}

// TestMCPBranchRejectsMalformed verifies branch shape validation
// catches clearly-invalid names before they reach git.
func TestMCPBranchRejectsMalformed(t *testing.T) {
	h := newMCPHarness(t, "BadBranch")
	projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("branch-bad-%d", nowNano()))

	for _, bad := range []string{"-leading-dash", "has space", "HEAD", "dots..in..name", "trailing/"} {
		res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
			"project_id": float64(projectID),
			"yaml":       "name: \"x\"\nversion: 1\ntasks:\n  - id: t\n    action: answer\n    prompt: \"x\"\n",
			"branch":     bad,
		})
		if err != nil {
			t.Fatalf("call %q: %v", bad, err)
		}
		// The formatter pattern wraps coordinator errors as
		// ✗-prefixed text rather than tool-level errors; assert
		// on the text so either shape counts as "rejected."
		text := mcpText(res)
		if !strings.Contains(text, "Failed to create run") {
			t.Errorf("expected branch %q to be rejected, got: %s", bad, text)
		}
	}
}

// TestMCPBranchClaimAndSubmitOnAutoBranch verifies a full
// claim + submit cycle on branch="auto". Before the PullBranch
// fix, the first claim on a fresh branch failed with
// "reference not found" because go-git's Pull rejects
// non-existent remote refs — the branch has no origin ref yet
// because nothing's been pushed to it. Now PullBranch
// ls-remotes first and treats "remote ref doesn't exist" as a
// soft no-op, letting first-submit create the ref naturally.
func TestMCPBranchClaimAndSubmitOnAutoBranch(t *testing.T) {
	h := newMCPHarness(t, "AutoBranchCycle")
	// Use the legacy zero-members test project path so we get
	// a real bare remote wired up via the test harness — the
	// membership-creator route goes through ~/.enju/repos which
	// isn't test-isolated.
	projectID := h.createTestProject()

	yaml := `name: "run on auto branch"
version: 1
tasks:
  - id: greet
    action: answer
    prompt: "Hi."
`
	// Create a run on an auto-allocated branch — nothing pushed
	// to origin/<branch> yet.
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "auto",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run branch=auto: %v / %s", err, mcpText(res))
	}

	// Point lastRunSeq at the freshly-created run. Single-run
	// project, so we grab it from the store.
	runs, _ := h.store.ListRunsByProject(projectID)
	if len(runs) != 1 {
		t.Fatalf("expected 1 run, got %d", len(runs))
	}
	h.lastProjectID = projectID
	h.lastRunSeq = runs[0].Seq
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runs[0].Seq)

	// Claim — the path that used to fail at PullBranch for a
	// fresh branch with no remote ref.
	claimRes := h.mcpClaimOK(t, "greet")
	if claimRes.IsError {
		t.Fatalf("claim on auto-branch run: %s", mcpText(claimRes))
	}

	// Submit — this push creates origin/<branch> for the first
	// time.
	h.mcpSubmitText(t, "greet", "Hi back.")

	// Task should now be accepted, and the branch ref should
	// exist on the remote.
	if got, _ := h.taskGet("greet")["state"].(string); got != "accepted" {
		t.Errorf("expected greet=accepted after submit, got %q", got)
	}

	// Verify the chosen branch ref actually landed on the bare
	// remote. Before the CheckoutBranch fix this assertion would
	// fail — the coordinator tracked branch="run-N" but commits
	// went to main because the worktree never switched.
	chosenBranch := runs[0].Branch
	remoteURL := h.remoteFor(projectID)
	assertRemoteHasBranch(t, remoteURL, chosenBranch)
	assertRemoteHasBranch(t, remoteURL, "main") // seed branch survives
}

// readRepoFileOnBranch reads a file from a specific branch on
// the bare remote. Needed by branch tests because the default
// readRepoFile clones the bare's HEAD (usually main), so
// content committed to a non-default branch looks "missing" to
// assertions that only check the default.
func readRepoFileOnBranch(t *testing.T, remoteURL, branch, repoRelPath string) ([]byte, bool) {
	t.Helper()
	cloneDir := t.TempDir()
	_, err := gogit.PlainClone(cloneDir, false, &gogit.CloneOptions{
		URL:           remoteURL,
		ReferenceName: plumbing.NewBranchReferenceName(branch),
		SingleBranch:  true,
	})
	if err != nil {
		return nil, false
	}
	b, err := os.ReadFile(filepath.Join(cloneDir, repoRelPath))
	if err != nil {
		return nil, false
	}
	return b, true
}

// assertRemoteHasBranch opens a bare repo and fails the test if
// `refs/heads/<branch>` isn't present. Used to pin the
// branch-per-run behavior at the actual git layer, not just the
// coordinator's bookkeeping.
func assertRemoteHasBranch(t *testing.T, remoteURL, branch string) {
	t.Helper()
	repo, err := gogit.PlainOpen(remoteURL)
	if err != nil {
		t.Fatalf("open bare %q: %v", remoteURL, err)
	}
	iter, err := repo.Branches()
	if err != nil {
		t.Fatalf("list branches on %q: %v", remoteURL, err)
	}
	defer iter.Close()
	found := false
	var names []string
	_ = iter.ForEach(func(ref *plumbing.Reference) error {
		name := ref.Name().Short()
		names = append(names, name)
		if name == branch {
			found = true
		}
		return nil
	})
	if !found {
		t.Fatalf("bare remote %q missing branch %q (have: %v)", remoteURL, branch, names)
	}
}

// TestMCPBranchTemplateModeCreatesRemoteRef is the tester's
// exact repro: enju_create_run with path=<template bundle>
// plus branch=<explicit>. The template-mode snapshot commit
// used to drop the branch argument to CommitFiles, so the
// snapshot landed on whatever branch the worktree was on
// (usually main) rather than on the run's branch. That left
// "run-N / experiment-X" as a coordinator label only — the
// bare remote never saw the ref.
func TestMCPBranchTemplateModeCreatesRemoteRef(t *testing.T) {
	h := newMCPHarness(t, "TemplateBranchRef")
	projectID := h.createTestProject()

	// Seed a trivial template bundle in the project's clone.
	h.writeRepoFiles(projectID, map[string]string{
		"enju_templates/hello/template.yaml": `name: "hello"
version: 1
tasks:
  - id: greet
    action: answer
    prompt: "Hi."
`,
	}, "seed hello template")

	// create_run via template path + explicit branch.
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/hello",
		"branch":     "experiment-1",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run template+branch: err=%v body=%s", err, mcpText(res))
	}

	// The snapshot commit ran inside create_run. The bare
	// remote must show refs/heads/experiment-1 already —
	// before any submit — since CommitFiles pushed.
	remoteURL := h.remoteFor(projectID)
	assertRemoteHasBranch(t, remoteURL, "experiment-1")
	assertRemoteHasBranch(t, remoteURL, "main")
}

// TestMCPBranchTemplateModeComputeExecutes closes the last
// square in the template-mode × branch test matrix: a compute
// task's script lives inside the template bundle, snapshotted
// into .enju/runs/{seq}/template/ at create_run time. If the
// snapshot lands on the wrong branch (the bug the
// CommitFilesRequest.Branch fix addressed), the executor
// wouldn't find the script at all — the task would fail with
// "script not found" instead of running.
//
// This test proves the executor + snapshot + branch routing
// all line up end-to-end on a non-default branch.
func TestMCPBranchTemplateModeComputeExecutes(t *testing.T) {
	h := newMCPHarness(t, "TemplateBranchCompute")
	projectID := h.createTestProject()

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"enju_templates/echo/template.yaml": {body: `name: "echo"
version: 1
tasks:
  - id: run
    action: compute
    script: scripts/echo.sh
`, mode: 0o644},
		"enju_templates/echo/scripts/echo.sh": {body: `#!/bin/bash
echo "branch-test-ran"
`, mode: 0o755},
	}, "seed echo template")

	// Template-mode create_run on a non-default branch.
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/echo",
		"branch":     "compute-branch",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run: err=%v body=%s", err, mcpText(res))
	}

	// Point the harness at this run.
	runs, _ := h.store.ListRunsByProject(projectID)
	h.lastProjectID = projectID
	h.lastRunSeq = runs[0].Seq
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runs[0].Seq)

	// Execute — this both claims and submits via the compute
	// executor path, which resolves `script:` from the per-
	// run snapshot dir. A broken snapshot branch would leave
	// the script missing.
	execRes := h.callOK(t, "enju_execute_task", map[string]any{
		"task_id": h.taskID("run"),
	})
	if execRes.IsError {
		t.Fatalf("execute_task failed — likely the template snapshot isn't on the run's branch: %s", mcpText(execRes))
	}

	// Remote should have both branches — main (from the seed)
	// and compute-branch (from the snapshot + submit).
	remoteURL := h.remoteFor(projectID)
	assertRemoteHasBranch(t, remoteURL, "main")
	assertRemoteHasBranch(t, remoteURL, "compute-branch")

	// Result content lives on compute-branch, not main — the
	// branch-aware read confirms the submit landed there.
	body, ok := readRepoFileOnBranch(t, remoteURL, "compute-branch", fmt.Sprintf(".enju/runs/%d/run/result.md", runs[0].Seq))
	if !ok {
		t.Fatalf("result.md missing from compute-branch")
	}
	if !strings.Contains(string(body), "branch-test-ran") {
		t.Errorf("expected script output in result; got:\n%s", string(body))
	}
}

// TestMCPBranchExplicitNameCreatesRemoteRef reproduces the
// tester-reported bug: coordinator accepts branch="experiment-1"
// on enju_create_run, serial-per-branch enforcement works, but
// commits land on main anyway because the git layer silently
// skips the branch switch. Must end with the bare remote having
// BOTH main (from the first run) and experiment-1 (from the
// second).
func TestMCPBranchExplicitNameCreatesRemoteRef(t *testing.T) {
	h := newMCPHarness(t, "ExplicitBranchRef")
	projectID := h.createTestProject()

	yaml := `name: "r"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
`
	// Run #1: default branch (main). Submit to get a real
	// commit on main so the bare has something.
	h.mcpCreateRunInline(t, projectID, yaml)
	h.mcpClaimOK(t, "t")
	h.mcpSubmitText(t, "t", "on main")

	// Run #2: explicit branch="experiment-1". Claim + submit.
	// The coordinator is fine with this (different branch from
	// run #1) and at the git level we expect the fat-client to
	// check out experiment-1 before committing, then push it.
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "experiment-1",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run branch=experiment-1: err=%v body=%s", err, mcpText(res))
	}
	runs, _ := h.store.ListRunsByProject(projectID)
	if len(runs) != 2 {
		t.Fatalf("expected 2 runs, got %d", len(runs))
	}
	// Point harness state at run #2.
	h.lastProjectID = projectID
	h.lastRunSeq = runs[1].Seq
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runs[1].Seq)

	h.mcpClaimOK(t, "t")
	h.mcpSubmitText(t, "t", "on experiment-1")

	// The critical assertion: the bare remote has
	// refs/heads/experiment-1, not just refs/heads/main.
	remoteURL := h.remoteFor(projectID)
	assertRemoteHasBranch(t, remoteURL, "main")
	assertRemoteHasBranch(t, remoteURL, "experiment-1")
}

// TestMCPDefaultBranchOnCreateProject verifies default_branch
// passed at create_project time sticks + runs without an
// explicit branch land on it.
func TestMCPDefaultBranchOnCreateProject(t *testing.T) {
	h := newMCPHarness(t, "DefaultBranch")
	projectName := fmt.Sprintf("branch-default-%d", nowNano())
	res, err := h.client.Call(context.Background(), "enju_create_project", map[string]any{
		"name":           projectName,
		"default_branch": "enju/work",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_project: err=%v isError=%v body=%s", err, res.IsError, mcpText(res))
	}
	proj, err := h.store.GetProjectByName(projectName)
	if err != nil || proj == nil {
		t.Fatalf("project lookup: %v", err)
	}
	if proj.DefaultBranch != "enju/work" {
		t.Fatalf("expected default_branch=\"enju/work\", got %q", proj.DefaultBranch)
	}

	// Run without explicit branch lands on enju/work.
	yaml := `name: "r"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
`
	h.callOK(t, "enju_create_run", map[string]any{
		"project_id": float64(proj.ID),
		"yaml":       yaml,
	})
	runs, _ := h.store.ListRunsByProject(proj.ID)
	if len(runs) != 1 || runs[0].Branch != "enju/work" {
		t.Fatalf("expected run on enju/work; got %+v", runs)
	}
}

// TestMCPSetProjectDefaultBranch verifies the tool flips the
// default for NEW runs (existing runs keep their branch) and
// is owner-only.
func TestMCPSetProjectDefaultBranch(t *testing.T) {
	h := newMCPHarness(t, "FlipDefault")
	projectID := mcpCreateProjectAs(t, h, h.client, fmt.Sprintf("branch-flip-%d", nowNano()))

	// Owner flips default → "develop".
	h.callOK(t, "enju_set_project_default_branch", map[string]any{
		"project_id": float64(projectID),
		"branch":     "develop",
	})
	if p, _ := h.store.GetProject(projectID); p == nil || p.DefaultBranch != "develop" {
		t.Fatalf("expected default_branch=\"develop\", got %+v", p)
	}

	// Non-owner (fresh member) is refused.
	bobUsername := h.register("Bob " + fmt.Sprintf("%d", nowNano()))
	h.callOK(t, "enju_add_project_member", map[string]any{
		"project_id": float64(projectID),
		"username":   bobUsername,
	})
	bob := newTestClientFor(t, h, bobUsername, "Bob")
	res, err := bob.Call(context.Background(), "enju_set_project_default_branch", map[string]any{
		"project_id": float64(projectID),
		"branch":     "other",
	})
	if err != nil {
		t.Fatalf("bob call: %v", err)
	}
	if !res.IsError {
		t.Fatalf("expected non-owner to be refused, got: %s", mcpText(res))
	}
	if !strings.Contains(mcpText(res), "owner") {
		t.Errorf("expected owner-only error, got: %s", mcpText(res))
	}
}
