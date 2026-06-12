package test

// Regression tests for longtask-parallel-run-desync.bug.md:
// `enju go --parallel N` on a fan-out where a compute task runs
// past the claim lease (defaultClaimTimeout, 30 min in production
// — multi-hour bio/ML batches blow it) left the run stuck. Two
// halves, each pinned by one test:
//
//  1. The reaper expired the in-flight claim and the long script's
//     completed work was refused → fixed by the claim heartbeat
//     (fat-client re-anchors the lease every lease/3 while the
//     script runs). TestMCPExecuteRunParallelLongTaskHeartbeatSurvivesReaper
//     drives the full loop against a REAL reaper.
//
//  2. The refusal itself was silently swallowed — coord.Post only
//     errors on transport failures, and the /result call site never
//     checked the body's {"error": ...} — so the task printed
//     "✓ completed" while the coordinator recorded nothing.
//     TestMCPExecuteRunForcedExpiryRefusalIsLoud force-expires the
//     claim and asserts the refusal now surfaces loudly.
//
// The "multi-hour script" is simulated deterministically: the slow
// script blocks on a sentinel file the test controls.

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/coordinator/engine"
	"github.com/enju-ai/enju/internal/coordinator/scheduler"
	"github.com/enju-ai/enju/internal/coordinator/store"
)

// waitForTask polls the store until cond(task) holds or the
// timeout elapses. Fails the test on timeout.
func waitForTask(t *testing.T, st store.CoordinatorStore, taskID string, timeout time.Duration, what string, cond func(*store.TaskRecord) bool) *store.TaskRecord {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		task, err := st.GetTask(taskID)
		if err == nil && task != nil && cond(task) {
			return task
		}
		time.Sleep(50 * time.Millisecond)
	}
	task, _ := st.GetTask(taskID)
	state := "<missing>"
	if task != nil {
		state = string(task.State)
	}
	t.Fatalf("timed out waiting for %s on %s (last state: %s)", what, taskID, state)
	return nil
}

// leaseDesyncFixture seeds a project with a gated slow script +
// fast scripts, a fan-out/fan-in workflow (slowTimeout empty =
// no timeout: line on the slow task), and starts the parallel
// cascade in the background. Returns the sentinel path that
// releases the slow script and the channel the cascade result
// lands on.
type execRunResult struct {
	text  string
	isErr bool
	err   error
}

func startLeaseDesyncRun(t *testing.T, h *mcpHarness, projectID int64, slowTimeout string) (gate string, done chan execRunResult) {
	t.Helper()
	gate = filepath.Join(t.TempDir(), "release-slow")

	h.writeRepoFilesWithMode(projectID, map[string]repoFileSpec{
		"scripts/fast.sh": {body: `#!/bin/bash
echo "fast $ENJU_TASK_ID"
`, mode: 0o755},
		"scripts/slow.sh": {body: fmt.Sprintf(`#!/bin/bash
# Stand-in for a multi-hour compute task: block until released.
while [ ! -f %q ]; do sleep 0.1; done
echo "slow done"
`, gate), mode: 0o755},
	}, "seed scripts")

	timeoutLine := ""
	if slowTimeout != "" {
		timeoutLine = fmt.Sprintf("\n    timeout: %s", slowTimeout)
	}
	yaml := fmt.Sprintf(`name: "lease-desync"
version: 1
tasks:
  - id: fast_a
    action: compute
    script: scripts/fast.sh
  - id: fast_b
    action: compute
    script: scripts/fast.sh
  - id: slow
    action: compute
    script: scripts/slow.sh%s
  - id: report
    action: compute
    script: scripts/fast.sh
    depends_on: [fast_a, fast_b, slow]
`, timeoutLine)
	h.mcpCreateRunInline(t, projectID, yaml)

	done = make(chan execRunResult, 1)
	go func() {
		res, err := h.client.Call(context.Background(), "enju_execute_run", map[string]any{
			"project_id": float64(projectID),
			"run_id":     float64(h.lastRunSeq),
			"parallel":   float64(3),
		})
		out := execRunResult{err: err}
		if res != nil {
			out.text = mcpText(res)
			out.isErr = res.IsError
		}
		done <- out
	}()
	return gate, done
}

func waitExecRunResult(t *testing.T, done chan execRunResult) execRunResult {
	t.Helper()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("execute_run transport error: %v", r.err)
		}
		return r
	case <-time.After(60 * time.Second):
		t.Fatal("execute_run did not return within 60s after slow script released")
		return execRunResult{}
	}
}

// TestMCPExecuteRunParallelLongTaskHeartbeatSurvivesReaper is the
// end-to-end fix verification: a REAL reaper sweeps every 200ms,
// the slow task's declared timeout (= claim lease) is 3s, and the
// script is held for ~7s — more than two full lease windows. The
// fat-client's heartbeat (lease/3 = 1s) must keep re-anchoring the
// deadline so the reaper never reaps the honest-but-slow worker;
// the run then drains to completed with every task recorded.
// Without the heartbeat the task flips to ready ~3s in and the run
// sticks (the field bug, in miniature).
func TestMCPExecuteRunParallelLongTaskHeartbeatSurvivesReaper(t *testing.T) {
	h := newMCPHarness(t, "ExecRunHeartbeat")
	projectID := h.createTestProject()

	// Production wiring without the escalator: plain expiry
	// (CLAIMED/RUNNING → READY) — the path that bit the field run.
	reaper := scheduler.NewReaper(h.store, 200*time.Millisecond,
		slog.New(slog.NewTextHandler(io.Discard, nil)))
	reaper.Start()
	t.Cleanup(reaper.Stop)

	gate, done := startLeaseDesyncRun(t, h, projectID, "3s")
	slowID := fmt.Sprintf("%d:1:slow", projectID)

	waitForTask(t, h.store, slowID, 30*time.Second, "running state", func(task *store.TaskRecord) bool {
		return task.State == store.TaskRunning
	})

	// Hold the script well past the 3s lease while the reaper
	// sweeps. The heartbeat must keep the claim alive the whole
	// time — if it doesn't, the task flips to ready within
	// lease + sweep-interval and we fail fast right here.
	hold := time.Now().Add(7 * time.Second)
	for time.Now().Before(hold) {
		task, err := h.store.GetTask(slowID)
		if err != nil || task == nil {
			t.Fatalf("fetch slow task during hold: %v", err)
		}
		if task.State != store.TaskRunning {
			t.Fatalf("slow task reaped despite heartbeat: state=%q ~%.1fs into a 3s lease",
				task.State, time.Until(hold).Seconds()-7)
		}
		time.Sleep(100 * time.Millisecond)
	}

	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatalf("writing release sentinel: %v", err)
	}
	r := waitExecRunResult(t, done)
	t.Logf("execute_run (isError=%v) output:\n%s", r.isErr, r.text)

	// All four tasks recorded, fan-in ran, run completed.
	for _, id := range []string{"fast_a", "fast_b", "slow", "report"} {
		full := fmt.Sprintf("%d:1:%s", projectID, id)
		task, err := h.store.GetTask(full)
		if err != nil || task == nil {
			t.Fatalf("fetch %s: %v", full, err)
		}
		if task.State != store.TaskAccepted {
			t.Errorf("%s state = %q, want %q", id, task.State, store.TaskAccepted)
		}
	}
	slowTask, _ := h.store.GetTask(slowID)
	run, err := h.store.GetRun(slowTask.RunID)
	if err != nil || run == nil {
		t.Fatalf("fetch run: %v", err)
	}
	if run.State != store.RunCompleted {
		t.Errorf("run state = %q, want %q", run.State, store.RunCompleted)
	}
}

// TestMCPExecuteRunForcedExpiryRefusalIsLoud pins the reporting
// half of the bug: if the claim IS lost mid-script (here: forced
// ExpireClaim, exactly the reaper's plan — in the wild: a network
// partition long enough for every heartbeat to miss), the
// coordinator refuses the eventual result report. That refusal
// must surface as a loud error entry — pre-fix it rendered as
// "✓ completed" and the run silently stuck at no_ready_compute.
func TestMCPExecuteRunForcedExpiryRefusalIsLoud(t *testing.T) {
	h := newMCPHarness(t, "ExecRunRefusalLoud")
	projectID := h.createTestProject()

	// No timeout: on the slow task → 30-min lease, heartbeats at
	// 10-min cadence — neither fires inside this test, so the
	// forced expiry below is the only lease event.
	gate, done := startLeaseDesyncRun(t, h, projectID, "")
	slowID := fmt.Sprintf("%d:1:slow", projectID)

	slowTask := waitForTask(t, h.store, slowID, 30*time.Second, "running state", func(task *store.TaskRecord) bool {
		return task.State == store.TaskRunning && task.ClaimedBy != 0
	})

	// Simulate the reaper firing mid-flight (same plan its plain-
	// expiry path applies).
	if _, err := h.store.ApplyPlan(store.Plan{
		Version: engine.EngineVersion,
		Mutations: []store.Mutation{
			store.ExpireClaim{TaskID: slowID, CitizenID: slowTask.ClaimedBy},
		},
	}); err != nil {
		t.Fatalf("expiring slow task's claim: %v", err)
	}
	waitForTask(t, h.store, slowID, 5*time.Second, "post-expiry ready state", func(task *store.TaskRecord) bool {
		return task.State == store.TaskReady
	})

	if err := os.WriteFile(gate, []byte("go"), 0o644); err != nil {
		t.Fatalf("writing release sentinel: %v", err)
	}
	r := waitExecRunResult(t, done)
	t.Logf("execute_run (isError=%v) output:\n%s", r.isErr, r.text)

	// The refusal is loud: an errored entry naming the refusal,
	// stop_reason=compute_errored — and ABOVE ALL no false "✓"
	// for work the coordinator never recorded.
	if strings.Contains(r.text, "✓ "+slowID) {
		t.Errorf("slow task rendered as ✓ completed despite the coordinator refusing its result:\n%s", r.text)
	}
	if !strings.Contains(r.text, "coordinator refused result") {
		t.Errorf("expected the refusal to surface in the entry reason, got:\n%s", r.text)
	}
	if !strings.Contains(r.text, "Stop reason: compute_errored") {
		t.Errorf("expected stop_reason=compute_errored on a refused report, got:\n%s", r.text)
	}
}
