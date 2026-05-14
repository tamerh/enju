package bots

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"
)

// writeLiveJSONL emulates notify's append-only writes to
// {projectDir}/.enju/events/live.jsonl. Tests use this to feed
// the tailer events without spinning up a real coord.
func writeLiveJSONL(t *testing.T, projectDir string, events ...tailEvent) {
	t.Helper()
	eventsDir := filepath.Join(projectDir, ".enju", "events")
	if err := os.MkdirAll(eventsDir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(eventsDir, "live.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	for _, ev := range events {
		raw, err := json.Marshal(ev)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.Write(append(raw, '\n')); err != nil {
			t.Fatal(err)
		}
	}
}

func TestAutoStop_TerminalEventStopsEligibleBot(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "auto", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "auto", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("auto", 5); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.WatchProjectEvents(ctx, projectDir, 7)

	// Feed a run_completed event for project 7, run seq 5.
	writeLiveJSONL(t, projectDir, tailEvent{
		Seq:  1,
		Type: "run_completed",
		Metadata: map[string]any{
			"from":    "active",
			"to":      "completed",
			"run_seq": float64(5),
		},
	})

	// The tailer polls every 200ms; give it generous slack to
	// observe the file, parse the event, and stop the bot.
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Status()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("bot still running after run_completed; status: %+v", s.Status())
}

func TestAutoStop_IgnoresOtherProjects(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "bot7", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "bot7", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("bot7", 5); err != nil {
		t.Fatal(err)
	}

	// Tailer wired to project 99; the event below carries
	// project 99 in the tailer's call, NOT project 7. bot7
	// should be untouched.
	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.WatchProjectEvents(ctx, projectDir, 99)

	writeLiveJSONL(t, projectDir, tailEvent{
		Seq:  1,
		Type: "run_completed",
		Metadata: map[string]any{
			"from":    "active",
			"to":      "completed",
			"run_seq": float64(5),
		},
	})

	time.Sleep(500 * time.Millisecond)
	if len(s.Status()) != 1 {
		t.Errorf("bot from a different project should not have been stopped; status: %+v", s.Status())
	}
}

func TestAutoStop_ConcurrentRunsHoldBotAlive(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "shared", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "shared", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	// Two concurrent auto-runs reference this bot.
	if err := s.MarkAutoRun("shared", 5); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("shared", 6); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.WatchProjectEvents(ctx, projectDir, 7)

	// Run 5 finishes first; bot must NOT stop.
	writeLiveJSONL(t, projectDir, tailEvent{
		Seq: 1, Type: "run_completed",
		Metadata: map[string]any{"run_seq": float64(5)},
	})
	time.Sleep(500 * time.Millisecond)
	if len(s.Status()) != 1 {
		t.Fatalf("bot stopped after first of two refs cleared; status: %+v", s.Status())
	}

	// Run 6 finishes; now the bot is the last-out and should stop.
	writeLiveJSONL(t, projectDir, tailEvent{
		Seq: 2, Type: "run_failed",
		Metadata: map[string]any{"run_seq": float64(6)},
	})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Status()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("bot still running after last ref cleared; status: %+v", s.Status())
}

func TestAutoStop_LeavesOperatorStartedBotsAlone(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	// Operator started this bot directly — StartedBy defaults
	// to "operator." The auto_bots flow rides along by marking
	// the run seq, but the bot is NOT eligible for auto-stop.
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "manual", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		ProjectID: 7, // StartedBy left blank → defaults to operator
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "manual", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("manual", 5); err != nil {
		t.Fatal(err)
	}

	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s.WatchProjectEvents(ctx, projectDir, 7)

	writeLiveJSONL(t, projectDir, tailEvent{
		Seq: 1, Type: "run_completed",
		Metadata: map[string]any{"run_seq": float64(5)},
	})

	time.Sleep(500 * time.Millisecond)
	if len(s.Status()) != 1 {
		t.Errorf("operator-started bot should not be auto-stopped; status: %+v", s.Status())
	}
	// Ref should be cleared even though the bot wasn't stopped —
	// useful so a later auto_bots run doesn't double-count.
	entry, err := readPIDFile(s.pidPathFor("manual"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entry.AutoRunIDs) != 0 {
		t.Errorf("AutoRunIDs should be cleared after terminal event, got %v", entry.AutoRunIDs)
	}
}

func TestAutoStop_WatchProjectEventsIsIdempotent(t *testing.T) {
	bin := writeFakeBinary(t, "")
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	projectDir := t.TempDir()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// Three calls; only one tailer goroutine should start.
	// (Hard to assert directly without exposing internals; the
	// observable signal is no panic and no extra resource use.)
	s.WatchProjectEvents(ctx, projectDir, 7)
	s.WatchProjectEvents(ctx, projectDir, 7)
	s.WatchProjectEvents(ctx, projectDir, 7)

	s.tailMu.Lock()
	defer s.tailMu.Unlock()
	if len(s.tailing) != 1 {
		t.Errorf("want 1 tailed project, got %d (%v)", len(s.tailing), s.tailing)
	}
}

func TestReconcile_DropsTerminalRefs(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "shared", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "shared", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	for _, seq := range []int64{5, 6, 7} {
		if err := s.MarkAutoRun("shared", seq); err != nil {
			t.Fatal(err)
		}
	}

	// Lookup says runs 5 and 7 are terminal; 6 is still active.
	terminalSet := map[int64]bool{5: true, 7: true}
	lookup := func(ctx context.Context, projectID, runSeq int64) (bool, error) {
		if projectID != 7 {
			t.Errorf("unexpected project_id in lookup: %d", projectID)
		}
		return terminalSet[runSeq], nil
	}
	if err := s.Reconcile(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	entry, err := readPIDFile(s.pidPathFor("shared"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{6}; !slices.Equal(entry.AutoRunIDs, want) {
		t.Errorf("after reconcile: want %v, got %v", want, entry.AutoRunIDs)
	}
	// Bot must NOT have been stopped — still has one live ref.
	if len(s.Status()) != 1 {
		t.Errorf("bot stopped despite live ref: %+v", s.Status())
	}
}

func TestReconcile_StopsWhenAllTerminal(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "stale", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "stale", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("stale", 5); err != nil {
		t.Fatal(err)
	}

	// Every run is terminal — pretends the fatclient crashed and
	// missed the run_completed events; reconcile should catch up.
	lookup := func(_ context.Context, _ int64, _ int64) (bool, error) {
		return true, nil
	}
	if err := s.Reconcile(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	// Give the reaper time to clean up after Stop.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Status()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("bot still running after reconcile cleared all refs; status: %+v", s.Status())
}

func TestReconcile_TreatsUnknownRunsAsTerminal(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "wiped", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "wiped", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("wiped", 42); err != nil {
		t.Fatal(err)
	}

	// Simulate a coord DB wipe: lookups for the (project, seq)
	// pair come back terminal=true (the IsRunTerminal contract).
	lookup := func(_ context.Context, _ int64, _ int64) (bool, error) {
		return true, nil
	}
	if err := s.Reconcile(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	// Bot should have been stopped — coord-wipe biases toward
	// releasing the bot rather than holding it forever waiting on
	// a run that no longer exists. Give the reaper time to run.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if len(s.Status()) == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Errorf("bot still running after coord-wipe reconcile; status: %+v", s.Status())
}

func TestReconcile_LookupErrorPreservesRef(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "transient", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		StartedBy: "auto_run", ProjectID: 7,
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "transient", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("transient", 5); err != nil {
		t.Fatal(err)
	}

	// Coord is temporarily unreachable. Reconcile must NOT drop
	// the ref — a transient network blip shouldn't kill auto-
	// managed bots; the next tailer-driven check will catch it.
	lookup := func(_ context.Context, _ int64, _ int64) (bool, error) {
		return false, fmt.Errorf("connection refused")
	}
	if err := s.Reconcile(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	entry, err := readPIDFile(s.pidPathFor("transient"))
	if err != nil {
		t.Fatal(err)
	}
	if want := []int64{5}; !slices.Equal(entry.AutoRunIDs, want) {
		t.Errorf("transient lookup error: want refs preserved (%v), got %v", want, entry.AutoRunIDs)
	}
	if len(s.Status()) != 1 {
		t.Errorf("bot stopped on lookup error: %+v", s.Status())
	}
}

func TestReconcile_LeavesOperatorBotAlone(t *testing.T) {
	bin := writeFakeBinary(t, `
echo "ready" > "$ENJU_BOT_PHASE_FILE"
`)
	s := newTestSupervisor(t, bin)
	defer s.StopAll(context.Background())

	// Operator-started bot with a stale auto_run_id from a
	// prior run-ride-along. Reconcile clears the ref but must
	// NOT stop the bot (manual wins).
	if _, _, err := s.Start(context.Background(), StartParams{
		BotName: "manual", WorkflowPath: "/tmp/p/workflow.yaml", Coordinator: "http://x",
		ProjectID: 7, // StartedBy left blank → operator
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.WaitForReady(context.Background(), "manual", 2*time.Second); err != nil {
		t.Fatal(err)
	}
	if err := s.MarkAutoRun("manual", 5); err != nil {
		t.Fatal(err)
	}

	lookup := func(_ context.Context, _ int64, _ int64) (bool, error) {
		return true, nil
	}
	if err := s.Reconcile(context.Background(), lookup); err != nil {
		t.Fatal(err)
	}
	if len(s.Status()) != 1 {
		t.Errorf("operator-started bot should not be auto-stopped: %+v", s.Status())
	}
	entry, _ := readPIDFile(s.pidPathFor("manual"))
	if len(entry.AutoRunIDs) != 0 {
		t.Errorf("AutoRunIDs should be cleared after reconcile, got %v", entry.AutoRunIDs)
	}
}

func TestRunSeqFromMetadata(t *testing.T) {
	cases := []struct {
		name string
		md   map[string]any
		want int64
	}{
		{"float64 (wire decode)", map[string]any{"run_seq": float64(42)}, 42},
		{"int64 (programmatic)", map[string]any{"run_seq": int64(7)}, 7},
		{"int (programmatic)", map[string]any{"run_seq": int(3)}, 3},
		{"absent", map[string]any{"from": "active"}, 0},
		{"nil", nil, 0},
		{"wrong type", map[string]any{"run_seq": "five"}, 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := runSeqFromMetadata(tc.md); got != tc.want {
				t.Errorf("want %d, got %d", tc.want, got)
			}
		})
	}
}
