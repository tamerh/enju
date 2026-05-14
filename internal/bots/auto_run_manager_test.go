package bots

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// newAutoRunTestSupervisor builds a Supervisor backed by a fake
// `enju` binary that simulates the daemon's stdin-EOF lifecycle
// plus the PhaseReady marker. Mirrors mcphandlers'
// newAutoBotsTestSupervisor; copied rather than imported so the
// bots package's tests don't depend on a parent.
func newAutoRunTestSupervisor(t *testing.T) *Supervisor {
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
	return NewSupervisorForTest(binPath, filepath.Join(dir, "pids"), filepath.Join(dir, "logs"))
}

func singleBotManifest(name string) *Manifest {
	return &Manifest{
		Version: 1,
		Bots:    []Bot{{Name: name, Model: "stub-model", Handler: "stub"}},
	}
}

// TestAutoRunManagerPreflightHappy — Preflight on a 1-bot
// manifest succeeds, AutoBotNames surfaces the bot, and Rollback
// stops it cleanly.
func TestAutoRunManagerPreflightHappy(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, 2*time.Second)
	if err := mgr.Preflight(context.Background(), singleBotManifest("alpha")); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if got := mgr.AutoBotNames(); len(got) != 1 || got[0] != "alpha" {
		t.Errorf("AutoBotNames: got %v, want [alpha]", got)
	}

	mgr.Rollback(context.Background())

	// After rollback, no bots should remain.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(sup.Status()) == 0 {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Errorf("after Rollback: bots remain: %+v", sup.Status())
}

// TestAutoRunManagerRollbackSparesOperatorBots — the REV.1
// invariant at the manager level. Construct: one operator-
// started bot (default StartedBy), one auto_run-started bot via
// the manager. Rollback. Operator bot must survive.
//
// This is the structural counterpart to mcphandlers'
// TestRollbackAutoStarts_SparesOperatorBots. The mcphandlers
// test pins the helper-level invariant (rollbackAutoStarts
// only stops what it's given); THIS test pins that the manager
// never gives Rollback the operator's slice — by construction,
// not by reviewer vigilance.
func TestAutoRunManagerRollbackSparesOperatorBots(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	// Operator-started (default StartedBy).
	if _, _, err := sup.Start(context.Background(), StartParams{
		BotName: "manual", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sup.WaitForReady(context.Background(), "manual", 2*time.Second); err != nil {
		t.Fatal(err)
	}

	// Now the manager spins up its own bot. Its Preflight call
	// must populate freshStarts with ONLY "fresh", never with
	// "manual" — even though "manual" is alive in the supervisor.
	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, 2*time.Second)
	if err := mgr.Preflight(context.Background(), singleBotManifest("fresh")); err != nil {
		t.Fatalf("Preflight: %v", err)
	}

	mgr.Rollback(context.Background())

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		var remaining []string
		for _, r := range sup.Status() {
			remaining = append(remaining, r.Name)
		}
		if len(remaining) == 1 && remaining[0] == "manual" {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("after Rollback: want only 'manual', got %+v", sup.Status())
}

// TestAutoRunManagerAlreadyRunningBotSurvivesRollback — second
// face of the same invariant. If the manager's Preflight finds
// a bot is already running (StartedFresh = false comes back
// from Supervisor.Start as AlreadyRunning), the bot should ride
// along in AutoBotNames but NOT in freshStarts. Rollback then
// leaves it alone.
func TestAutoRunManagerAlreadyRunningBotSurvivesRollback(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	// Pre-start "shared" as operator. Same name the manifest
	// below references.
	if _, _, err := sup.Start(context.Background(), StartParams{
		BotName: "shared", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := sup.WaitForReady(context.Background(), "shared", 2*time.Second); err != nil {
		t.Fatal(err)
	}

	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, 2*time.Second)
	if err := mgr.Preflight(context.Background(), singleBotManifest("shared")); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	if got := mgr.AutoBotNames(); len(got) != 1 || got[0] != "shared" {
		t.Errorf("AutoBotNames should include the AlreadyRunning bot: got %v", got)
	}

	mgr.Rollback(context.Background())

	// "shared" was AlreadyRunning, not in freshStarts, so
	// Rollback is a no-op for it. The bot stays alive.
	time.Sleep(200 * time.Millisecond)
	found := false
	for _, r := range sup.Status() {
		if r.Name == "shared" {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("AlreadyRunning bot was killed by Rollback (REV.1 regression)")
	}
}

// TestAutoRunManagerEmptyManifestRejected — defensive: callers
// must pre-check the manifest is non-empty (the user-facing
// "workflow declares no bots:" message fires there). The
// manager itself errors instead of silently no-op'ing so a
// future caller refactor can't bypass the check.
func TestAutoRunManagerEmptyManifestRejected(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, time.Second)
	if err := mgr.Preflight(context.Background(), nil); err == nil {
		t.Errorf("nil manifest: want error, got nil")
	}
	if err := mgr.Preflight(context.Background(), &Manifest{}); err == nil {
		t.Errorf("empty Bots: want error, got nil")
	}
}

// TestAutoRunManagerHookRunSeqMarksPidFile — HookRunSeq must
// actually write the run seq into each preflighted bot's pid
// file. This is the regression for "CLI's createRun never
// called HookRunSeq" (review issue #1, AGO-followup) — a
// passing call verifies the manager's side effects, not just
// that the function returns nil. Future regressions where
// HookRunSeq becomes a no-op surface here.
func TestAutoRunManagerHookRunSeqMarksPidFile(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	dir := t.TempDir()
	const seqUnderTest int64 = 42

	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, 2*time.Second)
	if err := mgr.Preflight(context.Background(), singleBotManifest("gamma")); err != nil {
		t.Fatalf("Preflight: %v", err)
	}
	unhooked := mgr.HookRunSeq(context.Background(), seqUnderTest, dir)
	if len(unhooked) > 0 {
		t.Fatalf("HookRunSeq: expected no unhooked bots, got %v", unhooked)
	}

	// Side-effect check: the bot's pid file should now carry
	// the run seq in its AutoRunIDs list. RunningBot doesn't
	// expose this field publicly (intentionally minimal), so
	// read the pid file directly — same shape readPIDFile
	// consumes everywhere else in the package.
	entry, rerr := readPIDFile(sup.pidPathFor("gamma"))
	if rerr != nil {
		t.Fatalf("readPIDFile: %v", rerr)
	}
	if !contains(entry.AutoRunIDs, seqUnderTest) {
		t.Errorf("AutoRunIDs missing %d: got %v", seqUnderTest, entry.AutoRunIDs)
	}
}

func contains(haystack []int64, needle int64) bool {
	for _, v := range haystack {
		if v == needle {
			return true
		}
	}
	return false
}

// TestAutoRunManagerHookRunSeqEmptyProjectDir — empty
// projectDir is a degraded but non-fatal path: HookRunSeq still
// marks pid files (the supervisor only needs projectDir for the
// tailer), but WatchProjectEvents logs and skips. Verify
// MarkAutoRun still fires.
func TestAutoRunManagerHookRunSeqEmptyProjectDir(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, 2*time.Second)
	if err := mgr.Preflight(context.Background(), singleBotManifest("delta")); err != nil {
		t.Fatal(err)
	}
	mgr.HookRunSeq(context.Background(), 99, "")
	entry, rerr := readPIDFile(sup.pidPathFor("delta"))
	if rerr != nil {
		t.Fatalf("readPIDFile: %v", rerr)
	}
	if !contains(entry.AutoRunIDs, 99) {
		t.Errorf("MarkAutoRun should fire even with empty projectDir; AutoRunIDs=%v", entry.AutoRunIDs)
	}
}

// TestAutoRunManagerRollbackIdempotent — Rollback drains
// freshStarts so a second call is a no-op. Important: the
// MCP handler's POST-failure path may invoke Rollback after a
// Preflight-failure path already ran (defense in depth). The
// double-rollback must not error or attempt to Stop bots a
// second time.
func TestAutoRunManagerRollbackIdempotent(t *testing.T) {
	sup := newAutoRunTestSupervisor(t)
	defer sup.StopAll(context.Background())

	mgr := NewAutoRunManager(sup, "/tmp/p/workflow.yaml", "http://x", 7, 2*time.Second)
	if err := mgr.Preflight(context.Background(), singleBotManifest("beta")); err != nil {
		t.Fatal(err)
	}
	mgr.Rollback(context.Background())
	// Second call must not panic or block.
	mgr.Rollback(context.Background())
}
