package bots

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
)

// newTestSupervisor builds a Supervisor pointed at a fake
// `enju` binary that simulates the daemon's stdin-EOF
// behavior. POSIX-only — windowsless test (the supervisor's
// production code is cross-platform; the harness depends on
// shell scripting).
func newTestSupervisor(t *testing.T, fakeBinary string) *Supervisor {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("supervisor test harness uses shell-script fake binary; POSIX only")
	}
	dir := t.TempDir()
	return &Supervisor{
		EnjuExec:        fakeBinary,
		PIDDir:          filepath.Join(dir, "pids"),
		LogDir:          filepath.Join(dir, "logs"),
		GracefulTimeout: 500 * time.Millisecond,
		procs:           make(map[string]*botProcess),
	}
}

// writeFakeBinary creates a shell script standing in for
// `enju bot run`. The script logs each invocation arg, then
// reads stdin until EOF (mimicking the real daemon's
// watchStdinEOF). Optional behavior knobs let individual tests
// inject delays or non-zero exits.
func writeFakeBinary(t *testing.T, behavior string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-enju")
	script := `#!/bin/sh
# Discriminate the supervisor's invocations: only react to
# 'bot run' subcommand. Other invocations are an error.
if [ "$1" != "bot" ] || [ "$2" != "run" ]; then
    echo "fake-enju: unexpected args: $@" 1>&2
    exit 99
fi
echo "fake-enju started: $@"
` + behavior + `
# Read stdin; exit when it closes (EOF).
while IFS= read -r line; do
    echo "got line: $line"
done
echo "fake-enju exiting cleanly on EOF"
`
	if err := os.WriteFile(path, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestSupervisor_StartAndStatus(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)

	rb, _, err := s.Start(context.Background(), StartParams{
		BotName:     "developer-bot",
		WorkflowPath: "/tmp/project/workflow.yaml",
		Coordinator: "http://localhost:8000",
	})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if rb.PID == 0 {
		t.Errorf("expected non-zero PID, got %d", rb.PID)
	}

	// Status reflects the running bot.
	st := s.Status()
	if len(st) != 1 || st[0].Name != "developer-bot" || st[0].PID != rb.PID {
		t.Errorf("Status: got %+v, want one entry for developer-bot/%d", st, rb.PID)
	}

	// PID file exists with correct fields.
	pidPath := filepath.Join(s.PIDDir, "developer-bot.json")
	data, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatalf("reading pid file: %v", err)
	}
	if !strings.Contains(string(data), "developer-bot") {
		t.Errorf("pid file missing bot name: %s", data)
	}

	// Cleanup.
	if _, err := s.Stop(context.Background(), "developer-bot"); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestSupervisor_DoubleStartIsIdempotent(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	first, outcome, err := s.Start(context.Background(), StartParams{
		BotName: "x", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	if outcome != StartedFresh {
		t.Errorf("first Start outcome: want StartedFresh, got %v", outcome)
	}
	var second *RunningBot
	second, outcome, err = s.Start(context.Background(), StartParams{
		BotName: "x", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	})
	if err != nil {
		t.Fatalf("second Start: want nil error (idempotent), got %v", err)
	}
	if outcome != AlreadyRunning {
		t.Errorf("second Start outcome: want AlreadyRunning, got %v", outcome)
	}
	if second.PID != first.PID {
		t.Errorf("second Start should return existing PID %d, got %d", first.PID, second.PID)
	}
}

func TestSupervisor_StopGracefulShutdown(t *testing.T) {
	// Fake daemon honors stdin-EOF (default behavior). Stop
	// closes stdin, waits, sees the daemon exit cleanly within
	// GracefulTimeout.
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)

	_, _, err := s.Start(context.Background(), StartParams{
		BotName: "x", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	if _, err := s.Stop(context.Background(), "x"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(start)
	// Should NOT have hit the 500ms graceful timeout.
	if elapsed >= 450*time.Millisecond {
		t.Errorf("Stop took %v — likely went through hard-kill path; expected fast graceful exit", elapsed)
	}

	// PID file should be removed by the reaper.
	if _, err := os.Stat(filepath.Join(s.PIDDir, "x.json")); !os.IsNotExist(err) {
		t.Errorf("pid file should be cleaned up after Stop, got stat err: %v", err)
	}
	// Status returns empty.
	if st := s.Status(); len(st) != 0 {
		t.Errorf("Status after Stop: got %+v, want empty", st)
	}
}

func TestSupervisor_StopHardKillFallback(t *testing.T) {
	// Fake daemon traps SIGPIPE and ignores stdin-EOF, simulating
	// a daemon hung mid-LLM that won't respond to graceful
	// shutdown. Supervisor should fall back to hard-kill after
	// the graceful timeout.
	bin := writeFakeBinary(t, `
# Trap and ignore the stdin-EOF signal — emulate a hung
# daemon that holds open the LLM call. Sleep is bounded so
# even on hard-kill failure the orphan exits within 5s.
trap '' PIPE
sleep 5 &
wait
`)
	s := newTestSupervisor(t, bin)

	_, _, err := s.Start(context.Background(), StartParams{
		BotName: "stuck-bot", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Override timeout to keep the test snappy.
	s.GracefulTimeout = 200 * time.Millisecond
	start := time.Now()
	if _, err := s.Stop(context.Background(), "stuck-bot"); err != nil {
		t.Errorf("Stop should still succeed via hard-kill: %v", err)
	}
	elapsed := time.Since(start)
	// Should be ≥ GracefulTimeout (we waited the timeout
	// before hard-killing) but not much more.
	if elapsed < 150*time.Millisecond {
		t.Errorf("Stop returned in %v — should have waited graceful timeout first", elapsed)
	}
	if elapsed > 5*time.Second {
		t.Errorf("Stop took too long: %v", elapsed)
	}
}

func TestSupervisor_StopUnknownBot(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	_, err := s.Stop(context.Background(), "ghost")
	if err == nil || !strings.Contains(err.Error(), "not running") {
		t.Errorf("expected not-running error, got: %v", err)
	}
}

func TestSupervisor_Logs(t *testing.T) {
	// Daemon prints two lines on startup; test that Logs reads them.
	bin := writeFakeBinary(t, `
echo "hello from fake bot"
echo "second line"
`)
	s := newTestSupervisor(t, bin)
	_, _, err := s.Start(context.Background(), StartParams{
		BotName: "logged", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	})
	if err != nil {
		t.Fatal(err)
	}

	// Wait for the daemon to write its startup lines. The log
	// is opened append-mode; we poll until at least one line
	// shows up to avoid racing the shell's buffered echo.
	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for time.Now().Before(deadline) {
		lines, _ = s.Logs("logged", 20)
		if len(lines) >= 2 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(lines) < 2 {
		t.Fatalf("expected at least 2 log lines, got %d: %v", len(lines), lines)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{"hello from fake bot", "second line"} {
		if !strings.Contains(joined, want) {
			t.Errorf("log missing %q:\n%s", want, joined)
		}
	}

	if _, err := s.Stop(context.Background(), "logged"); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

func TestSupervisor_LogsForUnstartedBot(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	lines, err := s.Logs("never-existed", 10)
	if err != nil {
		t.Errorf("expected nil error for missing log, got: %v", err)
	}
	if len(lines) != 0 {
		t.Errorf("expected empty lines for unstarted bot, got: %v", lines)
	}
}

func TestSupervisor_StopAll(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)

	for _, name := range []string{"a", "b", "c"} {
		if _, _, err := s.Start(context.Background(), StartParams{
			BotName: name, WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(s.Status()); got != 3 {
		t.Errorf("Status before StopAll: got %d, want 3", got)
	}
	errs := s.StopAll(context.Background())
	if len(errs) != 0 {
		t.Errorf("StopAll errors: %v", errs)
	}
	if got := len(s.Status()); got != 0 {
		t.Errorf("Status after StopAll: got %d, want 0", got)
	}
}

func TestSupervisor_StopReportsGracefulFlag(t *testing.T) {
	// Graceful path → Graceful=true.
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "g", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Fatal(err)
	}
	res, err := s.Stop(context.Background(), "g")
	if err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if !res.Graceful {
		t.Errorf("expected Graceful=true on clean exit, got %+v", res)
	}

	// Hard-kill path → Graceful=false.
	hangBin := writeFakeBinary(t, `
trap '' PIPE
sleep 5 &
wait
`)
	s2 := newTestSupervisor(t, hangBin)
	s2.GracefulTimeout = 100 * time.Millisecond
	if _, _, err := s2.Start(context.Background(), StartParams{
		BotName: "h", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Fatal(err)
	}
	res, err = s2.Stop(context.Background(), "h")
	if err != nil {
		t.Fatalf("Stop hard-kill should succeed: %v", err)
	}
	if res.Graceful {
		t.Errorf("expected Graceful=false on hard-kill, got %+v", res)
	}
}

func TestSupervisor_RecentExitsRecordsCrash(t *testing.T) {
	// Daemon exits with non-zero status. RecentExits should
	// list it with reason starting "crashed:".
	bin := writeFakeBinary(t, `
echo "about to crash"
exit 7
`)
	s := newTestSupervisor(t, bin)
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "crashy", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Fatal(err)
	}
	// Wait for the reaper to record the exit.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if exits := s.RecentExits(); len(exits) > 0 {
			ev := exits[0]
			if ev.Name != "crashy" {
				t.Errorf("name: got %q", ev.Name)
			}
			if !strings.HasPrefix(ev.Reason, "crashed:") {
				t.Errorf("reason: got %q, want crashed: prefix", ev.Reason)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("RecentExits never received the crash event; current: %+v", s.RecentExits())
}

func TestSupervisor_StaleHostPIDPruned(t *testing.T) {
	// Pre-seed the pid dir with an entry whose PID is
	// definitely dead (PID 99999 — beyond typical Linux
	// pid_max defaults). NewSupervisor should walk the dir
	// and remove it.
	dir := t.TempDir()
	pidDir := filepath.Join(dir, "pids")
	if err := os.MkdirAll(pidDir, 0700); err != nil {
		t.Fatal(err)
	}
	stalePath := filepath.Join(pidDir, "ghost-bot.json")
	body := []byte(`{"name":"ghost-bot","pid":99999,"started_at":"2020-01-01T00:00:00Z"}`)
	if err := os.WriteFile(stalePath, body, 0600); err != nil {
		t.Fatal(err)
	}
	// Construct supervisor with custom dirs (NewSupervisor
	// would point at ~/.enju/bots/pids, which we don't want
	// to mutate from a test).
	s := &Supervisor{PIDDir: pidDir, LogDir: filepath.Join(dir, "logs")}
	s.pruneStalePIDFiles()
	if _, err := os.Stat(stalePath); !os.IsNotExist(err) {
		t.Errorf("expected stale pid file to be pruned, got stat err: %v", err)
	}
}

func TestSupervisor_StartAllPartialFailure(t *testing.T) {
	// Two bots in the manifest; the first starts fine, the
	// second's binary errors. Verify the supervisor returns
	// per-bot results without short-circuiting.
	dir := t.TempDir()
	// Fake binary that succeeds for bot=a, fails for bot=b.
	bin := filepath.Join(dir, "selective-fake")
	script := `#!/bin/sh
case "$3" in
    --bot=b) echo "fake-enju refusing bot b" 1>&2; exit 7 ;;
esac
echo "fake-enju started: $@"
while IFS= read -r line; do : ; done
`
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatal(err)
	}
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	// Start a, expect success.
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "a", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Errorf("Start a: %v", err)
	}
	// Start b — the supervisor itself doesn't reject; the
	// daemon process just exits non-zero. Reaper records the
	// crash and clears the entry.
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "b", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Errorf("Start b (process spawned even if it'll exit): %v", err)
	}
	// Wait for b's reaper to fire.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		gone := true
		for _, rb := range s.Status() {
			if rb.Name == "b" {
				gone = false
				break
			}
		}
		if gone {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// a should still be running, b should have crashed (and
	// be in RecentExits).
	running := s.Status()
	if len(running) != 1 || running[0].Name != "a" {
		t.Errorf("running: got %+v, want one entry for a", running)
	}
	exits := s.RecentExits()
	foundB := false
	for _, e := range exits {
		if e.Name == "b" && strings.HasPrefix(e.Reason, "crashed:") {
			foundB = true
		}
	}
	if !foundB {
		t.Errorf("expected b to appear in RecentExits with crashed:; got %+v", exits)
	}
}

func TestSupervisor_AllowToolsForwardedToDaemon(t *testing.T) {
	// Pin the trust-model wiring: when AllowTools is set on
	// StartParams, the supervisor passes --allow-tools=... to
	// the daemon's argv. The fake binary echoes its args so
	// we can grep the log for the flag.
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName:      "checker",
		WorkflowPath: "/tmp/p/workflow.yaml",
		Coordinator:  "http://x",
		AllowTools:   []string{"Read", "Grep", "Glob"},
	}); err != nil {
		t.Fatal(err)
	}
	// Wait for the fake binary to echo its args.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		lines, _ := s.Logs("checker", 50)
		joined := strings.Join(lines, "\n")
		if strings.Contains(joined, "--allow-tools=Read,Grep,Glob") {
			_, _ = s.Stop(context.Background(), "checker")
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("--allow-tools never appeared in daemon argv (log: %v)", func() string {
		l, _ := s.Logs("checker", 50)
		return strings.Join(l, "\n")
	}())
}

func TestSupervisor_ReapsOnDaemonExit(t *testing.T) {
	// Daemon exits on its own (no stdin-EOF dance needed).
	// Supervisor's reapOnExit should clean up the map.
	bin := writeFakeBinary(t, `
echo "exiting on my own"
exit 0
`)
	s := newTestSupervisor(t, bin)
	_, _, err := s.Start(context.Background(), StartParams{
		BotName: "self-exit", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	})
	if err != nil {
		t.Fatal(err)
	}
	// Wait for the reaper to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Status()) == 0 {
			return // reaped
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("reaper didn't clean up after self-exit; status: %+v", s.Status())
}

// TestSupervisor_ConcurrentStartIsAtomic is the regression for
// the REV.2 race: pre-fix, Start released procsMu between the
// existence check and the procs-map insert, so two concurrent
// Start("foo") callers could both pass the check, both fork,
// and the second insert would silently leak the first daemon.
// Fixed by holding procsMu for the entire function.
//
// The test fires N goroutines, all calling Start("same-bot") at
// once, then asserts that exactly one returns StartedFresh and
// the rest return AlreadyRunning with the same PID. Without
// the fix, multiple goroutines would observe StartedFresh.
func TestSupervisor_ConcurrentStartIsAtomic(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	const goroutines = 8
	type result struct {
		rb      *RunningBot
		outcome StartOutcome
		err     error
	}
	results := make(chan result, goroutines)
	start := make(chan struct{})
	for i := 0; i < goroutines; i++ {
		go func() {
			<-start
			rb, outcome, err := s.Start(context.Background(), StartParams{
				BotName: "racey", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
			})
			results <- result{rb, outcome, err}
		}()
	}
	close(start)

	freshCount := 0
	alreadyCount := 0
	var freshPID int
	for i := 0; i < goroutines; i++ {
		r := <-results
		if r.err != nil {
			t.Fatalf("Start err: %v", r.err)
		}
		switch r.outcome {
		case StartedFresh:
			freshCount++
			freshPID = r.rb.PID
		case AlreadyRunning:
			alreadyCount++
			// AlreadyRunning callers must surface the same PID
			// as the one that actually spawned — without that,
			// callers ref-counting by PID would split-brain.
			if r.rb.PID == 0 {
				t.Errorf("AlreadyRunning surfaced zero PID")
			}
		}
	}
	if freshCount != 1 {
		t.Errorf("want exactly 1 StartedFresh across %d concurrent Start calls, got %d (alreadyCount=%d)",
			goroutines, freshCount, alreadyCount)
	}
	if alreadyCount != goroutines-1 {
		t.Errorf("want %d AlreadyRunning, got %d", goroutines-1, alreadyCount)
	}
	if freshPID == 0 {
		t.Errorf("StartedFresh surfaced zero PID")
	}
}

func TestSupervisor_PIDFileRecordsStartedBy(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	// Operator-start defaults StartedBy to "operator" so an
	// auto_bots run that rides along can't accidentally claim
	// the bot as auto-managed.
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "manual", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Fatal(err)
	}
	entry, err := readPIDFile(s.pidPathFor("manual"))
	if err != nil {
		t.Fatalf("read manual pid: %v", err)
	}
	if entry.StartedBy != "operator" {
		t.Errorf("manual start: want StartedBy=operator, got %q", entry.StartedBy)
	}

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "auto", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run",
	}); err != nil {
		t.Fatal(err)
	}
	entry, err = readPIDFile(s.pidPathFor("auto"))
	if err != nil {
		t.Fatalf("read auto pid: %v", err)
	}
	if entry.StartedBy != "auto_run" {
		t.Errorf("auto start: want StartedBy=auto_run, got %q", entry.StartedBy)
	}
}

func TestSupervisor_MarkUnmarkAutoRun(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "b", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run",
	}); err != nil {
		t.Fatal(err)
	}

	// Mark adds; re-marking the same seq is a no-op.
	if err := s.MarkAutoRun("b", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("b", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("b", 2); err != nil {
		t.Fatal(err)
	}
	entry, _ := readPIDFile(s.pidPathFor("b"))
	if want := []int64{1, 2}; !slices.Equal(entry.AutoRunIDs, want) {
		t.Errorf("after mark 1,1,2: want %v, got %v", want, entry.AutoRunIDs)
	}

	// Unmark removes the seq; unmarking a missing seq is a no-op.
	if err := s.UnmarkAutoRun("b", 1); err != nil {
		t.Fatal(err)
	}
	if err := s.UnmarkAutoRun("b", 99); err != nil {
		t.Fatal(err)
	}
	entry, _ = readPIDFile(s.pidPathFor("b"))
	if want := []int64{2}; !slices.Equal(entry.AutoRunIDs, want) {
		t.Errorf("after unmark 1,99: want %v, got %v", want, entry.AutoRunIDs)
	}

	// Unmarking a missing pid file is benign — auto-stop is
	// best-effort cleanup, not a strict invariant.
	if err := s.UnmarkAutoRun("never-started", 5); err != nil {
		t.Errorf("unmark on missing pid: want nil, got %v", err)
	}
}

func TestSupervisor_PhaseAndWaitForReady_Succeeds(t *testing.T) {
	// Fake daemon writes "ready" to ENJU_BOT_PHASE_FILE then
	// holds open on stdin (mimics a real daemon entering the
	// poll loop). Supervisor.WaitForReady should observe the
	// transition.
	bin := writeFakeBinary(t, `
if [ -n "$ENJU_BOT_PHASE_FILE" ]; then
    echo "ready" > "$ENJU_BOT_PHASE_FILE"
fi
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "r", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "r", 2*time.Second); err != nil {
		t.Fatalf("WaitForReady: %v", err)
	}
	got, err := s.Phase("r")
	if err != nil {
		t.Fatal(err)
	}
	if got != PhaseReady {
		t.Errorf("Phase after wait: want %q, got %q", PhaseReady, got)
	}
}

func TestSupervisor_WaitForReady_Timeout(t *testing.T) {
	// Fake daemon never writes a phase marker. WaitForReady
	// should fail with the last-seen phase in the message — the
	// diagnostic that lets the operator distinguish "stuck in
	// self-heal" from "process never even started."
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "stuck", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run",
	}); err != nil {
		t.Fatal(err)
	}
	err := s.WaitForReady(context.Background(), "stuck", 200*time.Millisecond)
	if err == nil {
		t.Fatal("WaitForReady: want timeout error, got nil")
	}
	if !strings.Contains(err.Error(), "did not reach ready phase") {
		t.Errorf("error should explain the wait failed: %v", err)
	}
	if !strings.Contains(err.Error(), "last seen") {
		t.Errorf("error should include last-seen phase for diagnosis: %v", err)
	}
}

func TestSupervisor_PhaseFile_RemovedOnDaemonExit(t *testing.T) {
	// Fake daemon writes ready, then exits cleanly. Supervisor's
	// reaper should drop the phase file alongside the pid file
	// so the next Start doesn't see a stale "ready" before its
	// fresh process has actually reached the loop.
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "exit", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "exit", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Stop(context.Background(), "exit"); err != nil {
		t.Fatal(err)
	}
	// Allow the reaper to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(s.phasePathFor("exit")); os.IsNotExist(err) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Errorf("phase file %s still present after daemon exit", s.phasePathFor("exit"))
}

func TestSupervisor_EligibleForAutoStop(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	cases := []struct {
		name      string
		startedBy string
		runs      []int64
		want      bool
	}{
		{"auto, empty list", "auto_run", nil, true},
		{"auto, one ref", "auto_run", []int64{1}, false},
		{"operator, empty list", "operator", nil, false},
		{"operator, one ref", "operator", []int64{1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := s.Start(context.Background(), StartParams{
				BotName: tc.name, WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
				StartedBy: tc.startedBy,
			}); err != nil {
				t.Fatal(err)
			}
			for _, seq := range tc.runs {
				if err := s.MarkAutoRun(tc.name, seq); err != nil {
					t.Fatal(err)
				}
			}
			got, err := s.EligibleForAutoStop(tc.name)
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Errorf("eligible: want %v, got %v", tc.want, got)
			}
		})
	}

	// Missing pid file → not eligible (nothing to stop).
	got, err := s.EligibleForAutoStop("never-existed")
	if err != nil {
		t.Fatal(err)
	}
	if got {
		t.Errorf("eligible for missing pid: want false, got true")
	}
}
