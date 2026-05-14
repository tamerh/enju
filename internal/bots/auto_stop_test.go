package bots

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
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
