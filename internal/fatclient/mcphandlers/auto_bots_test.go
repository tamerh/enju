package mcphandlers

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/bots"
)

// newTestSupervisor builds a Supervisor pointed at a fake
// `enju` binary that simulates the daemon's stdin-EOF behavior
// plus the NDA.2 phase-file write. POSIX-only — same pattern
// as internal/bots's test harness but local copy so we don't
// import the bots test package (which doesn't export).
func newAutoBotsTestSupervisor(t *testing.T) *bots.Supervisor {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("supervisor test harness uses shell-script fake binary; POSIX only")
	}
	dir := t.TempDir()
	binPath := filepath.Join(dir, "fake-enju")
	script := `#!/bin/sh
if [ "$1" != "bot" ] || [ "$2" != "run" ]; then
    echo "fake-enju: unexpected args: $@" 1>&2
    exit 99
fi
echo "fake-enju started: $@"
if [ -n "$ENJU_BOT_PHASE_FILE" ]; then
    echo "ready" > "$ENJU_BOT_PHASE_FILE"
fi
while IFS= read -r line; do
    echo "got line: $line"
done
echo "fake-enju exiting cleanly on EOF"
`
	if err := os.WriteFile(binPath, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return bots.NewSupervisorForTest(binPath, filepath.Join(dir, "pids"), filepath.Join(dir, "logs"))
}

func TestAutoBotsReadyTimeout_Default(t *testing.T) {
	// Clear any inherited env so the test sees the production
	// default (30s) regardless of the shell that started it.
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "")
	got := autoBotsReadyTimeout()
	if want := 30 * time.Second; got != want {
		t.Errorf("default timeout: want %s, got %s", want, got)
	}
}

func TestAutoBotsReadyTimeout_EnvOverride(t *testing.T) {
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "5s")
	if got := autoBotsReadyTimeout(); got != 5*time.Second {
		t.Errorf("env=5s: want 5s, got %s", got)
	}
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "2m")
	if got := autoBotsReadyTimeout(); got != 2*time.Minute {
		t.Errorf("env=2m: want 2m, got %s", got)
	}
}

func TestAutoBotsReadyTimeout_BadValueFallsBackToDefault(t *testing.T) {
	// A malformed duration shouldn't crash create_run; falls back
	// to the safe default so the operator just sees the standard
	// 30s wait instead of a hard failure at the wait site.
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "not-a-duration")
	if got := autoBotsReadyTimeout(); got != 30*time.Second {
		t.Errorf("bad value: want 30s default, got %s", got)
	}
	t.Setenv("ENJU_AUTO_BOTS_TIMEOUT", "-5s")
	if got := autoBotsReadyTimeout(); got != 30*time.Second {
		t.Errorf("negative value: want 30s default, got %s", got)
	}
}

// TestRollbackAutoStarts_SparesOperatorBots is the regression
// for the REV.1 bug: the post-POST rollback path used to pass
// the full autoBotNames list (including AlreadyRunning operator
// bots) to Stop, killing manual bots that an unrelated coord
// failure on this run shouldn't touch. Fixed by routing both
// rollback sites through rollbackAutoStarts with the strict
// StartedFresh subset.
//
// Setup: start one bot manually (StartedBy defaults to
// operator), one auto_run-flagged. Call rollbackAutoStarts
// with ONLY the auto bot. The operator bot must survive.
func TestRollbackAutoStarts_SparesOperatorBots(t *testing.T) {
	sup := newAutoBotsTestSupervisor(t)
	defer sup.StopAll(context.Background())

	// Operator-started bot (default StartedBy).
	if _, _, err := sup.Start(context.Background(), bots.StartParams{
		BotName: "manual", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	// Fresh auto_run bot.
	if _, _, err := sup.Start(context.Background(), bots.StartParams{
		BotName: "fresh", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		ProjectID: 7, StartedBy: "auto_run",
	}); err != nil {
		t.Fatal(err)
	}
	if err := sup.WaitForReady(context.Background(), "manual", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := sup.WaitForReady(context.Background(), "fresh", 2*time.Second); err != nil {
		t.Fatal(err)
	}

	// Mimic the post-POST rollback caller — pass ONLY the
	// fresh-start subset. If a future refactor reintroduces the
	// bug (passes the wider autoBotNames list), this test fails
	// because "manual" gets stopped.
	rollbackAutoStarts(context.Background(), sup, []string{"fresh"})

	// Give the reaper time to clean up.
	deadline := time.Now().Add(2 * time.Second)
	var remaining []string
	for time.Now().Before(deadline) {
		remaining = nil
		for _, r := range sup.Status() {
			remaining = append(remaining, r.Name)
		}
		if len(remaining) == 1 && remaining[0] == "manual" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("after rollback: want only 'manual' running, got %v", remaining)
}

// TestEnjuAutoBotsTimeoutEnvVarName guards against an accidental
// rename of the env var — the documented name is part of the
// public surface (operators set it in their shell rc).
func TestEnjuAutoBotsTimeoutEnvVarName(t *testing.T) {
	// Direct os.Setenv via t.Setenv guarantees cleanup; this
	// also serves as living documentation for the var name.
	const want = "ENJU_AUTO_BOTS_TIMEOUT"
	t.Setenv(want, "42s")
	if got := os.Getenv(want); got != "42s" {
		t.Fatalf("env round-trip via %q failed", want)
	}
	if got := autoBotsReadyTimeout(); got != 42*time.Second {
		t.Errorf("autoBotsReadyTimeout didn't honor %s=42s: %s", want, got)
	}
}
