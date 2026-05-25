package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/coord"
	"github.com/enju-ai/enju/internal/fatclient/service"
)

func TestDriveStopClassifiers(t *testing.T) {
	fatal := []string{service.StopComputeErrored, service.StopContextCancelled, service.StopComputeFailed, service.StopGitOperationFailed}
	for _, s := range fatal {
		if !driveStopIsFatal(s) {
			t.Errorf("driveStopIsFatal(%q) = false, want true", s)
		}
	}
	for _, s := range []string{service.StopNoReadyCompute, service.StopAsyncTaskStarted, service.StopMaxTasks, service.StopCitizenTaskReady} {
		if driveStopIsFatal(s) {
			t.Errorf("driveStopIsFatal(%q) = true, want false", s)
		}
	}
	for _, s := range []string{service.StopCitizenTaskReady, service.StopComputeAssignedElsewhere} {
		if !driveStopIsGate(s) {
			t.Errorf("driveStopIsGate(%q) = false, want true", s)
		}
	}
	for _, s := range []string{service.StopNoReadyCompute, service.StopAsyncTaskStarted, service.StopComputeFailed} {
		if driveStopIsGate(s) {
			t.Errorf("driveStopIsGate(%q) = true, want false", s)
		}
	}
	if driveExit(true) != 1 || driveExit(false) != 0 {
		t.Errorf("driveExit: want 1/0 for true/false, got %d/%d", driveExit(true), driveExit(false))
	}
}

// driveTestServer scripts the coord endpoints driveRun touches: the run
// detail (state), the ready-tasks scan, and the run task list. ready
// and runState are read fresh per request so a test can flip them; an
// empty ready list + no compute means ExecuteRun returns
// no_ready_compute without executing any script (no workspace needed).
type driveTestServer struct {
	runStateJSON string // body for GET /projects/P/runs/S
	readyJSON    string // body for GET /tasks/ready
	tasksJSON    string // body for GET /projects/P/runs/S/tasks
}

func newDriveSession(t *testing.T, s *driveTestServer) *cliSession {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.Contains(r.URL.Path, "/tasks/ready"):
			_, _ = w.Write([]byte(s.readyJSON))
		case strings.HasSuffix(r.URL.Path, "/tasks"):
			_, _ = w.Write([]byte(s.tasksJSON))
		default: // run detail
			_, _ = w.Write([]byte(s.runStateJSON))
		}
	}))
	t.Cleanup(srv.Close)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	fc := service.New(service.Config{
		Coord:         coord.New(coord.Config{BaseURL: srv.URL, Username: "u", AuthToken: "t", Logger: logger}),
		WorkspaceRoot: t.TempDir(), // wires s.enjugit; reconcile no-ops on the missing clone
		Logger:        logger,
	})
	return &cliSession{FC: fc, URL: srv.URL}
}

func driveParams() service.ExecuteRunParams {
	return service.ExecuteRunParams{ProjectID: 12, RunSeq: 7, MaxTasks: 10, Parallel: 1, KeepGoing: true}
}

// A terminal run exits 0 on the first pass without waiting (the loop's
// terminal check fires before the interval).
func TestDriveRun_TerminalExitsCleanly(t *testing.T) {
	sess := newDriveSession(t, &driveTestServer{
		runStateJSON: `{"id":1,"project_id":12,"seq":7,"state":"completed","branch":"run-7"}`,
		readyJSON:    `[]`,
		tasksJSON:    `[]`,
	})
	code := driveRun(context.Background(), sess, driveParams(), time.Millisecond, false, true)
	if code != 0 {
		t.Errorf("terminal run: exit %d, want 0", code)
	}
}

// A run that's idle (no ready compute) but NOT terminal and with nothing
// in flight is a stall — drive reports it and exits 1 rather than
// spinning forever.
func TestDriveRun_StallExitsNonZero(t *testing.T) {
	sess := newDriveSession(t, &driveTestServer{
		runStateJSON: `{"id":1,"project_id":12,"seq":7,"state":"active","branch":"run-7"}`,
		readyJSON:    `[]`,
		tasksJSON:    `[]`, // nothing running
	})
	code := driveRun(context.Background(), sess, driveParams(), time.Millisecond, false, true)
	if code != 1 {
		t.Errorf("stalled run: exit %d, want 1", code)
	}
}

// A citizen gate stops drive (it's compute-only) and exits 0 when no
// compute failed.
func TestDriveRun_CitizenGateStops(t *testing.T) {
	sess := newDriveSession(t, &driveTestServer{
		runStateJSON: `{"id":1,"project_id":12,"seq":7,"state":"active","branch":"run-7"}`,
		readyJSON:    `[{"id":"12:7:review_a","action":"review","seq":1}]`,
		tasksJSON:    `[]`,
	})
	code := driveRun(context.Background(), sess, driveParams(), time.Millisecond, false, true)
	if code != 0 {
		t.Errorf("citizen-gated run: exit %d, want 0", code)
	}
}

// --once runs a single reap+launch pass and returns even on a
// non-terminal run (cron-tick semantics), without entering the wait.
func TestDriveRun_OnceSinglePass(t *testing.T) {
	sess := newDriveSession(t, &driveTestServer{
		runStateJSON: `{"id":1,"project_id":12,"seq":7,"state":"active","branch":"run-7"}`,
		readyJSON:    `[]`,
		tasksJSON:    `[{"id":"12:7:s1","state":"running"}]`, // in flight → not a stall
	})
	done := make(chan int, 1)
	go func() { done <- driveRun(context.Background(), sess, driveParams(), time.Hour, true, true) }()
	select {
	case code := <-done:
		if code != 0 {
			t.Errorf("--once: exit %d, want 0", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("--once did not return promptly — it must not enter the interval wait")
	}
}
