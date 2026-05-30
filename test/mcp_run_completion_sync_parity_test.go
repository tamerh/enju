package test

import (
	"fmt"
	"strings"
	"testing"

	"github.com/enju-ai/enju/internal/testutil/gittest"
)

// TestMCPRunCompletionSyncParity pins the invariant that broke in
// production: run-completion sync (the workflow's sync: setting —
// FF-merge the run branch into the default branch) must fire on
// whichever execution path completes the run's final task.
//
// The regression: applyRunCompletion was wired only into the
// citizen single-submit path. The compute path (reached by both
// enju_execute_task and the enju_execute_run cascade) and the
// citizen batch-submit path ran the accepted-merges step but never
// run-completion sync — so a compute-final pipeline completed yet
// its branch silently never merged into the default branch,
// regardless of sync:. It stayed invisible because citizen-final
// workflows (which DO go through the wired path) still synced.
//
// Each subtest runs an equivalent run with sync:merge on a forked
// run branch and asserts the SAME observable on the operator's
// local clone (sync:merge is local-only by design): the default
// branch advanced AND carries the run's tracked marker. The
// citizen_submit subtest is the parity reference — the path that
// always worked — guarding that the fix did not regress it.
func TestMCPRunCompletionSyncParity(t *testing.T) {
	// Two chained compute tasks; the last writes a tracked
	// marker. sync:merge is local-only, so the observable is the
	// operator clone's refs/heads/main, not the bare remote.
	const computeYAML = `name: "sync parity compute"
version: 1
publish:
  mode: local
tasks:
  - id: seed
    action: compute
    script: scripts/seed.sh
    prompt: "seed"
  - id: emit
    action: compute
    script: scripts/emit.sh
    depends_on: [seed]
    writes:
      - out/marker.txt
    prompt: "emit"
`
	const seedSh = "#!/bin/bash\necho seeded\n"
	const emitSh = "#!/bin/bash\nmkdir -p out\nprintf 'PARITY_MARKER\\n' > out/marker.txt\necho done\n"

	// seedComputeRun: fresh project + bundle + a run on a FORKED
	// run branch (branch=auto, so the run branch is distinct from
	// the default branch and sync has something to merge). Returns
	// the operator clone dir and the pre-run main SHA.
	seedComputeRun := func(t *testing.T, h *mcpHarness) (cloneDir, mainBefore string) {
		t.Helper()
		projectID := h.createTestProject()
		// script: paths are project-root-relative — seed.sh / emit.sh
		// land at <project>/scripts/, not under the workflow's template
		// dir. Only the YAML itself goes inside enju/templates/p/.
		h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
			"enju/templates/p/enju.yaml": {body: computeYAML, mode: 0o644},
			"scripts/seed.sh":            {body: seedSh, mode: 0o755},
			"scripts/emit.sh":            {body: emitSh, mode: 0o755},
		}, "seed compute bundle")
		h.callOK(t, "enju_create_run", map[string]any{
			"project_id": float64(projectID),
			"path":       "enju/templates/p",
			"branch":     "auto",
		})
		h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:seed", projectID))
		cloneDir = h.workspaceDirForProject(projectID)
		mainBefore = strings.TrimSpace(gittest.Run(t, cloneDir, "rev-parse", "refs/heads/main"))
		if mainBefore == "" {
			t.Fatal("operator clone has no refs/heads/main before run")
		}
		return cloneDir, mainBefore
	}

	// assertSynced: after a completed run with sync:merge, the
	// operator clone's main must have advanced AND carry the run's
	// tracked marker (merged in from the run branch).
	assertSynced := func(t *testing.T, cloneDir, mainBefore string) {
		t.Helper()
		mainAfter := strings.TrimSpace(gittest.Run(t, cloneDir, "rev-parse", "refs/heads/main"))
		if mainAfter == mainBefore {
			t.Fatalf("run completed but the default branch never advanced — "+
				"run-completion sync did not fire (before=%s after=%s)",
				mainBefore, mainAfter)
		}
		marker, err := gittest.RunOK(t, cloneDir, "show", "refs/heads/main:out/marker.txt")
		if err != nil {
			t.Fatalf("default branch advanced but tracked marker out/marker.txt "+
				"is not on main — sync incomplete: %v", err)
		}
		if !strings.Contains(marker, "PARITY_MARKER") {
			t.Fatalf("marker on main has unexpected content: %q", marker)
		}
	}

	// execute_run: the exact production repro — a compute-only run
	// driven to completion by the enju_execute_run cascade.
	t.Run("execute_run", func(t *testing.T) {
		h := newMCPHarness(t, "SyncParityExecRun")
		cloneDir, mainBefore := seedComputeRun(t, h)
		res := h.callOK(t, "enju_execute_run", map[string]any{
			"project_id": float64(h.lastProjectID),
			"run_id":     float64(h.lastRunSeq),
		})
		if txt := mcpText(res); !strings.Contains(txt, "no_ready_compute") {
			t.Fatalf("expected cascade to drain to completion, got:\n%s", txt)
		}
		assertSynced(t, cloneDir, mainBefore)
	})

	// execute_task: the same workflow driven one task at a time
	// through the single-task compute path (also previously missing
	// the sync step).
	t.Run("execute_task", func(t *testing.T) {
		h := newMCPHarness(t, "SyncParityExecTask")
		cloneDir, mainBefore := seedComputeRun(t, h)
		for _, def := range []string{"seed", "emit"} {
			h.callOK(t, "enju_execute_task", map[string]any{"task_id": h.taskID(def)})
		}
		assertSynced(t, cloneDir, mainBefore)
	})

	// citizen_submit_reference: the single citizen-submit path
	// always carried run-completion sync. Same observable end
	// state proves parity; this also guards against the fix
	// regressing the path that already worked.
	t.Run("citizen_submit_reference", func(t *testing.T) {
		h := newMCPHarness(t, "SyncParityCitizen")
		projectID := h.createTestProject()
		const citizenYAML = `name: "sync parity citizen"
version: 1
publish:
  mode: local
tasks:
  - id: emit
    action: answer
    prompt: "Emit the marker."
    writes:
      - out/marker.txt
`
		h.callOK(t, "enju_create_run", map[string]any{
			"project_id": float64(projectID),
			"yaml":       citizenYAML,
			"branch":     "auto",
		})
		h.rememberRunFromTaskID(t, fmt.Sprintf("%d:1:emit", projectID))
		cloneDir := h.workspaceDirForProject(projectID)
		mainBefore := strings.TrimSpace(gittest.Run(t, cloneDir, "rev-parse", "refs/heads/main"))
		h.mcpClaimOK(t, "emit")
		h.mcpSubmitArtifacts(t, "emit", "marker emitted", map[string]string{
			"out/marker.txt": "PARITY_MARKER\n",
		})
		assertSynced(t, cloneDir, mainBefore)
	})
}
