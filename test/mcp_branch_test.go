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

// TestMCPBranchAutoAllocation verifies branch="auto" picks
// unused slots: inline-YAML runs walk run-1, run-2, ...;
// template-mode runs would use the bundle dir name as slug
// (covered separately by TestMCPBranchAutoTemplateSlug).
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

// TestMCPBranchAutoTemplateSlug verifies branch="auto" derives
// its slug from the template bundle dir name rather than the
// generic "run-N" — so `path="enju_templates/hello"` + auto
// yields "hello-1", "hello-2", .... Makes parallel parameter
// sweeps instantly recognizable in `git branch`.
func TestMCPBranchAutoTemplateSlug(t *testing.T) {
	h := newMCPHarness(t, "AutoTemplateSlug")
	projectID := h.createTestProject()

	h.writeRepoFiles(projectID, map[string]string{
		"enju_templates/hello/template.yaml": `name: "hello"
version: 1
tasks:
  - id: greet
    action: answer
    prompt: "Hi."
`,
	}, "seed hello template")

	// Two template-mode runs with branch="auto" should produce
	// hello-1 and hello-2.
	for i := 0; i < 2; i++ {
		res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
			"project_id": float64(projectID),
			"path":       "enju_templates/hello",
			"branch":     "auto",
		})
		if err != nil || res.IsError {
			t.Fatalf("create_run auto #%d: err=%v body=%s", i, err, mcpText(res))
		}
	}

	runs, err := h.store.ListRunsByProject(projectID)
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	got := map[string]bool{}
	for _, r := range runs {
		got[r.Branch] = true
	}
	if !got["hello-1"] || !got["hello-2"] {
		t.Errorf("expected hello-1 and hello-2 auto branches; got=%+v", got)
	}

	// The create_run text should surface the resolved branch so
	// callers see what got picked without shelling out to git.
	res, _ := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/hello",
		"branch":     "auto",
	})
	text := mcpText(res)
	if !strings.Contains(text, `branch "hello-3"`) {
		t.Errorf("expected create_run text to cite branch \"hello-3\", got: %s", text)
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

// TestMCPBranchExportDiagramRoutesToRunBranch pins the
// tester-reported bug where enju_export_diagram committed the
// .mmd file to workspace HEAD instead of the run's branch.
// Root cause: handleGetRun's response was missing the `branch`
// field, so runBranchFromData returned empty →
// CheckoutBranch("") fell back to the project default.
func TestMCPBranchExportDiagramRoutesToRunBranch(t *testing.T) {
	h := newMCPHarness(t, "ExportDiagramBranch")
	projectID := h.createTestProject()

	h.mcpCreateRunInline(t, projectID, `name: "r"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
`)
	h.mcpClaimOK(t, "t")
	h.mcpSubmitText(t, "t", "on main")

	// Run #2 on explicit branch.
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml": `name: "r2"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
`,
		"branch": "experiment-1",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run: err=%v body=%s", err, mcpText(res))
	}
	runs, _ := h.store.ListRunsByProject(projectID)
	h.lastProjectID = projectID
	h.lastRunSeq = runs[1].Seq
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runs[1].Seq)

	h.callOK(t, "enju_export_diagram", map[string]any{
		"project_id": float64(projectID),
		"run_id":     float64(runs[1].Seq),
		"phase":      "initial",
	})

	remoteURL := h.remoteFor(projectID)
	expPath := fmt.Sprintf(".enju/runs/%d/graph/initial.mmd", runs[1].Seq)
	if _, ok := readRepoFileOnBranch(t, remoteURL, "experiment-1", expPath); !ok {
		t.Errorf("diagram missing from experiment-1 — wrong branch routing")
	}
	if _, onMain := readRepoFileOnBranch(t, remoteURL, "main", expPath); onMain {
		t.Errorf("diagram leaked onto main — should live only on experiment-1")
	}
}

// TestMCPBranchUpstreamSubstitutionOnExplicitBranch is the
// tester's regression repro: a two-task run (second reads
// first.content) on an explicit non-default branch. Claim of
// second must see the substituted prompt, not a literal
// {{first.content}}. Root cause candidate: the workspace ends
// up on a different branch than expected after submit, so the
// fat-client resolver can't find first's commit in the local
// clone.
func TestMCPBranchUpstreamSubstitutionOnExplicitBranch(t *testing.T) {
	h := newMCPHarness(t, "UpstreamOnBranch")
	projectID := h.createTestProject()

	yaml := `name: "two-step"
version: 1
tasks:
  - id: first
    action: answer
    prompt: "Say a fruit."
  - id: second
    action: answer
    prompt: "Echo upstream: {{first.content}}"
`
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "workbranch",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run: err=%v body=%s", err, mcpText(res))
	}

	runs, _ := h.store.ListRunsByProject(projectID)
	h.lastProjectID = projectID
	h.lastRunSeq = runs[0].Seq
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runs[0].Seq)

	h.mcpClaimOK(t, "first")
	h.mcpSubmitText(t, "first", "apple")

	// Claim second — the MCP client resolves {{first.content}}
	// locally by reading first's result.md at first's commit
	// SHA. Pre-regression this worked; pre-fix the claim
	// returned the literal {{first.content}} token and the
	// get_task_inputs call errored with "no result file found".
	claimRes := h.mcpClaimOK(t, "second")
	text := mcpText(claimRes)
	if strings.Contains(text, "{{first.content}}") {
		t.Errorf("substitution regression — claim returned literal {{first.content}}; got:\n%s", text)
	}
	if !strings.Contains(text, "apple") {
		t.Errorf("expected substituted prompt with 'apple'; got:\n%s", text)
	}
}

// TestMCPBranchUpstreamSubstitutionAfterBranchChurn tries to
// reproduce the tester's regression with more realistic branch
// churn: multiple runs on different branches, then a
// substitution-dependent task pair. The hypothesis is that
// CheckoutBranch's Force:true wipes the worktree tree enough
// that the resolver can't find a prior commit's blob.
func TestMCPBranchUpstreamSubstitutionAfterBranchChurn(t *testing.T) {
	h := newMCPHarness(t, "UpstreamAfterChurn")
	projectID := h.createTestProject()

	// Run #1 on lane-a (some prior branch work).
	runA := mcpCreateRunOnBranch(t, h, projectID, "lane-a")
	h.lastProjectID = projectID
	h.lastRunSeq = runA
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runA)
	h.mcpClaimOK(t, "t")
	h.mcpSubmitText(t, "t", "prior work on lane-a")

	// Run #2 on a fresh branch with two dependent tasks.
	yaml := `name: "chain"
version: 1
tasks:
  - id: first
    action: answer
    prompt: "Say a fruit."
  - id: second
    action: answer
    prompt: "Echo: {{first.content}}"
`
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml":       yaml,
		"branch":     "chain-branch",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run chain: err=%v body=%s", err, mcpText(res))
	}
	runs, _ := h.store.ListRunsByProject(projectID)
	// The chain run is the most recent.
	chainSeq := runs[len(runs)-1].Seq
	h.lastRunSeq = chainSeq
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, chainSeq)

	h.mcpClaimOK(t, "first")
	h.mcpSubmitText(t, "first", "apple")

	// Claim second. The resolver reads first's result.md at
	// first's commit SHA — if CheckoutBranch's Force:true
	// wiped the worktree such that first's commit isn't
	// reachable from the current clone, this fails.
	claimRes := h.mcpClaimOK(t, "second")
	text := mcpText(claimRes)
	if strings.Contains(text, "{{first.content}}") {
		t.Errorf("regression — second's prompt has literal {{first.content}}; full:\n%s", text)
	}
	if strings.Contains(text, "no result file found") {
		t.Errorf("regression — resolver couldn't find first's result file; full:\n%s", text)
	}
	if !strings.Contains(text, "apple") {
		t.Errorf("expected substituted prompt with 'apple'; got:\n%s", text)
	}
}

// TestMCPBranchForksFromProjectBaseNotWorkspaceHEAD pins the
// tester-reported ancestry bug. Pre-fix, switching between
// branches would compound history — `lane-b` created after
// `lane-a` inherited lane-a's commits because CheckoutBranch
// forked from current workspace HEAD. Post-fix, new branches
// fork from `origin/main` (the project base), not from HEAD.
func TestMCPBranchForksFromProjectBaseNotWorkspaceHEAD(t *testing.T) {
	h := newMCPHarness(t, "BranchAncestry")
	projectID := h.createTestProject()

	runA := mcpCreateRunOnBranch(t, h, projectID, "lane-a")
	h.lastProjectID = projectID
	h.lastRunSeq = runA
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runA)
	h.mcpClaimOK(t, "t")
	h.mcpSubmitText(t, "t", "on lane-a")

	runB := mcpCreateRunOnBranch(t, h, projectID, "lane-b")
	h.lastRunSeq = runB
	h.lastRunID = fmt.Sprintf("%d:%d", projectID, runB)
	h.mcpClaimOK(t, "t")
	h.mcpSubmitText(t, "t", "on lane-b")

	remoteURL := h.remoteFor(projectID)
	runAResultPath := fmt.Sprintf(".enju/runs/%d/t/result.md", runA)
	if _, leaked := readRepoFileOnBranch(t, remoteURL, "lane-b", runAResultPath); leaked {
		t.Errorf("lane-b unexpectedly contains run-A's result — branch forked from lane-a instead of main")
	}
	if _, ok := readRepoFileOnBranch(t, remoteURL, "lane-a", runAResultPath); !ok {
		t.Errorf("lane-a missing its own result — something is off")
	}
}

// mcpCreateRunOnBranch is a tiny helper for branch tests.
func mcpCreateRunOnBranch(t *testing.T, h *mcpHarness, projectID int64, branch string) int {
	t.Helper()
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"yaml": `name: "r"
version: 1
tasks:
  - id: t
    action: answer
    prompt: "x"
`,
		"branch": branch,
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run %q: err=%v body=%s", branch, err, mcpText(res))
	}
	runs, _ := h.store.ListRunsByProject(projectID)
	return runs[len(runs)-1].Seq
}

// TestMCPBranchTemplateUntrackedAutoCommitsToDefault is the
// tester's exact repro: an UNTRACKED template file in the MCP
// workspace, then create_run with a non-default branch. Pre-fix,
// the untracked file was silently swept onto the run's branch
// only — subsequent runs on other branches saw "template not
// found." Post-fix, EnsureBundleOnDefault auto-commits the
// bundle to the default branch first, so it's reusable.
func TestMCPBranchTemplateUntrackedAutoCommitsToDefault(t *testing.T) {
	h := newMCPHarness(t, "TemplateUntracked")
	projectID := h.createTestProject()

	// Force the MCP workspace to clone the project so we have
	// a real local worktree to write an untracked file into.
	remoteURL := h.remoteFor(projectID)
	proj, err := h.workspace.ForProject(projectID, remoteURL, "")
	if err != nil {
		t.Fatalf("open workspace: %v", err)
	}

	// Write the template UNTRACKED directly into the worktree
	// — bypassing git entirely, the way a user authoring a
	// template with a plain text editor would.
	bundleDir := filepath.Join(proj.WorkDir(), "enju_templates", "hello")
	if err := os.MkdirAll(bundleDir, 0o755); err != nil {
		t.Fatalf("mkdir bundle: %v", err)
	}
	tmplPath := filepath.Join(bundleDir, "template.yaml")
	tmplBody := `name: "hello"
version: 1
tasks:
  - id: greet
    action: answer
    prompt: "Hi."
`
	if err := os.WriteFile(tmplPath, []byte(tmplBody), 0o644); err != nil {
		t.Fatalf("write template: %v", err)
	}

	// create_run on a non-default branch. The handler should
	// auto-commit the untracked template to main BEFORE
	// branching off to experiment-1.
	res, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/hello",
		"branch":     "experiment-1",
	})
	if err != nil || res.IsError {
		t.Fatalf("create_run: err=%v body=%s", err, mcpText(res))
	}

	// Template must be committed on main in the bare remote
	// — the auto-commit is the whole point.
	mainBody, ok := readRepoFileOnBranch(t, remoteURL, "main", "enju_templates/hello/template.yaml")
	if !ok {
		t.Fatalf("template missing from main — auto-commit did not fire")
	}
	if !strings.Contains(string(mainBody), "Hi.") {
		t.Errorf("main template content looks wrong: %s", string(mainBody))
	}

	// And experiment-1 exists (carries its per-run snapshot).
	assertRemoteHasBranch(t, remoteURL, "experiment-1")

	// The real test: a SECOND run on a different branch can
	// still find the template (it's on main now). Pre-fix,
	// this failed with "template not found."
	res2, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/hello",
		"branch":     "experiment-2",
	})
	if err != nil || res2.IsError {
		t.Fatalf("second create_run on experiment-2: err=%v body=%s", err, mcpText(res2))
	}
	assertRemoteHasBranch(t, remoteURL, "experiment-2")
}

// TestMCPBranchTemplateReusableAcrossBranches is the tester's
// follow-up repro: a template authored in the worktree must be
// reusable across multiple runs on different branches. Pre-fix,
// the untracked template file got swept onto whichever branch
// first used it (the snapshot's catch-all AddGlob), so run #2
// on a different branch saw "template not found".
//
// The invariant: templates live on the project's default
// branch. Run #1 on any branch auto-commits the template to
// default before branching off; run #2 on another branch reads
// it from default's already-committed history.
func TestMCPBranchTemplateReusableAcrossBranches(t *testing.T) {
	h := newMCPHarness(t, "TemplateReusable")
	projectID := h.createTestProject()

	// Seed the template in the project's clone (simulating the
	// user writing the file into their workspace before first
	// create_run). writeRepoFiles commits to main, which is the
	// default branch for createTestProject — exactly the
	// "templates live on default" end state, but reached via
	// the normal authoring path.
	h.writeRepoFiles(projectID, map[string]string{
		"enju_templates/hello/template.yaml": `name: "hello"
version: 1
tasks:
  - id: greet
    action: answer
    prompt: "Hi."
`,
	}, "seed hello template")

	// Run #1 on an explicit non-default branch.
	res1, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/hello",
		"branch":     "experiment-1",
	})
	if err != nil || res1.IsError {
		t.Fatalf("create_run #1 on experiment-1: err=%v body=%s", err, mcpText(res1))
	}

	// Run #2 on a DIFFERENT non-default branch. This is the
	// critical second call — pre-fix it would have failed
	// with "template not found" because the template only
	// lived on experiment-1.
	res2, err := h.client.Call(context.Background(), "enju_create_run", map[string]any{
		"project_id": float64(projectID),
		"path":       "enju_templates/hello",
		"branch":     "experiment-2",
	})
	if err != nil || res2.IsError {
		t.Fatalf("create_run #2 on experiment-2: err=%v body=%s", err, mcpText(res2))
	}

	// Both branches exist, template is on main.
	remoteURL := h.remoteFor(projectID)
	assertRemoteHasBranch(t, remoteURL, "main")
	assertRemoteHasBranch(t, remoteURL, "experiment-1")
	assertRemoteHasBranch(t, remoteURL, "experiment-2")
	mainBody, ok := readRepoFileOnBranch(t, remoteURL, "main", "enju_templates/hello/template.yaml")
	if !ok {
		t.Fatalf("template missing from main — expected auto-commit to default")
	}
	if !strings.Contains(string(mainBody), "Hi.") {
		t.Errorf("main's template content looks wrong: %s", string(mainBody))
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
