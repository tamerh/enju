package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// cmdDrive is `enju drive <seq>`: drive an EXISTING run to terminal by
// looping reap → launch → wait until nothing is left to do. This is the
// headless async self-driver and the re-attach path.
//
// Why it exists: ExecuteRun (what `enju resume` / `enju go` call once)
// LAUNCHES ready compute and the reaper FINISHES detached async tasks,
// but nothing autonomously alternates the two headless — so a multi-
// stage async pipeline stalls after the first wave unless a human keeps
// poking resume or an MCP session's ticker happens to be running. drive
// is that loop, decoupled from the bot supervisor (contrast `enju go
// --auto-agents`, which is for citizen-gated runs).
//
// For a pure sync run the first pass drains it inline and the run is
// terminal immediately — drive behaves like `enju resume` and exits in
// one pass. The loop only spins while async work is in flight.
func cmdDrive(args []string) {
	fs := flag.NewFlagSet("drive", flag.ExitOnError)
	projectID := fs.Int64("project", 0, "Operate on a specific project id (default: the project that owns the current directory)")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	parallel := fs.Int("parallel", 1, "Run up to N compute tasks concurrently per launch pass (default 1 = serial). Capped at 32.")
	maxTasks := fs.Int("max-tasks", 1000, "Cap on compute tasks launched per pass (safety net)")
	keepGoing := fs.Bool("keep-going", true, "On a compute failure, record it and keep driving the rest of the run (default). Fix it and `enju retry <id>`.")
	failFast := fs.Bool("fail-fast", false, "Stop at the first compute failure (overrides --keep-going).")
	interval := fs.Duration("interval", 5*time.Second, "Wait between launch passes while async work is in flight. Ignored with --once.")
	once := fs.Bool("once", false, "Run exactly one reap+launch pass and exit (cron-friendly), instead of looping until the run is terminal.")
	asJSON := fs.Bool("json", false, "Stream per-task status as JSONL on stdout")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: enju drive <seq> [--parallel N] [--max-tasks N] [--interval 5s] [--once] [--project id] [--json]")
		os.Exit(2)
	}

	parallelN, perr := normalizeParallelFlag(*parallel)
	if perr != nil {
		fmt.Fprintf(os.Stderr, "--parallel: %v\n", perr)
		os.Exit(2)
	}

	sess := openCLISession(*coordOverride)

	projID, seq, rerr := resolveResumeTarget(sess, *projectID, fs.Arg(0))
	if rerr != nil {
		fmt.Fprintf(os.Stderr, "drive: %v\n", rerr)
		os.Exit(2)
	}

	ctx := context.Background()
	os.Exit(driveRun(ctx, sess, service.ExecuteRunParams{
		ProjectID: int(projID),
		RunSeq:    seq,
		MaxTasks:  *maxTasks,
		Parallel:  parallelN,
		KeepGoing: *keepGoing && !*failFast,
	}, *interval, *once, *asJSON))
}

// driveRun is the loop. Each iteration: REAP (turn any finished async
// subprocess into coord state — also the re-attach pickup on the first
// pass), LAUNCH (ExecuteRun drains ready compute, incl. tasks the reap
// just unblocked), then decide whether to stop. Returns the process
// exit code (1 if any task failed or a driver/gate stop, else 0).
func driveRun(ctx context.Context, sess *cliSession, p service.ExecuteRunParams, interval time.Duration, once, asJSON bool) int {
	anyFailed := false
	var lastStop, lastBlocker string

	for {
		// ④/re-attach REAP: pick up async that finished during the
		// wait (or before we attached). Best-effort.
		sess.FC.ReconcileRunSeq(ctx, int64(p.ProjectID), int64(p.RunSeq))

		// ① LAUNCH.
		res, err := sess.FC.ExecuteRun(ctx, p)
		if err != nil {
			fmt.Fprintf(os.Stderr, "drive: execute run: %v\n", err)
			return 1
		}
		if shouldRenderPoll(res, lastStop, lastBlocker) {
			renderExecuteResult(os.Stdout, res, asJSON)
		}
		lastStop = res.StopReason
		lastBlocker = blockerTaskID(res)
		if len(failedTaskIDs(res)) > 0 {
			anyFailed = true
		}

		// A driver-level error (or, under --fail-fast, a task failure)
		// stops everything — no progress is possible.
		if driveStopIsFatal(res.StopReason) {
			return 1
		}
		// A citizen gate is not something drive can advance (it's
		// compute-only; gates are `enju go --auto-agents`' job).
		if driveStopIsGate(res.StopReason) {
			if res.Blocker != nil {
				fmt.Fprintf(os.Stderr, "drive: stopped at %s gate %s — nothing more to drive (use `enju go --auto-agents` for citizen-gated runs)\n",
					res.Blocker.Action, res.Blocker.TaskID)
			}
			return driveExit(anyFailed)
		}

		// ② TERMINAL? The run reaches a terminal state only once every
		// task settles — an in-flight async task keeps it non-terminal,
		// so this alone is a sufficient "everything done" signal.
		terminal, terr := sess.isRunTerminal(ctx, int64(p.ProjectID), int64(p.RunSeq))
		if terr == nil && terminal {
			logf(asJSON, "▶ run %d:%d terminal", p.ProjectID, p.RunSeq)
			return driveExit(anyFailed)
		}
		if once {
			return driveExit(anyFailed)
		}

		// Stall guard: nothing ready to launch AND nothing in flight to
		// reap, yet the run isn't terminal — a reap won't reveal new
		// work, so looping would spin forever. Surface it (likely a
		// self-held stuck claim, already listed by the renderer, or a
		// gate elsewhere) and exit rather than hang.
		if res.StopReason == service.StopNoReadyCompute {
			if running, rerr := sess.FC.CountRunningTasks(ctx, p.ProjectID, p.RunSeq); rerr == nil && running == 0 {
				fmt.Fprintf(os.Stderr,
					"drive: run %d:%d is idle but not terminal — no ready compute and nothing in flight. Likely a stuck claim or a gate; nothing to drive.\n",
					p.ProjectID, p.RunSeq)
				return 1
			}
		}

		// ③ WAIT (async is cooking). Ctrl-C exits cleanly; detached
		// jobs keep running and can be re-attached.
		select {
		case <-ctx.Done():
			fmt.Fprintln(os.Stderr, "drive: interrupted; detached async jobs keep running — re-attach with `enju drive <seq>`")
			return 1
		case <-time.After(interval):
		}
	}
}

// driveStopIsFatal reports stop reasons that end the drive with a
// failure: a driver-level error / cancellation, or — only reachable
// under --fail-fast, since keep-going downgrades them — a task-level
// compute/git failure.
func driveStopIsFatal(stop string) bool {
	switch stop {
	case service.StopComputeErrored, service.StopContextCancelled,
		service.StopComputeFailed, service.StopGitOperationFailed:
		return true
	}
	return false
}

// driveStopIsGate reports stop reasons where the next ready task is a
// human/citizen action (or compute assigned elsewhere) that drive
// cannot itself advance.
func driveStopIsGate(stop string) bool {
	return stop == service.StopCitizenTaskReady || stop == service.StopComputeAssignedElsewhere
}

// driveExit maps the accumulated failure state to an exit code: 1 if
// any task failed across the drive (keep-going records them and
// continues), else 0.
func driveExit(anyFailed bool) int {
	if anyFailed {
		return 1
	}
	return 0
}

// blockerTaskID is the render-dedupe key for the current blocker.
func blockerTaskID(res *service.ExecuteRunResult) string {
	if res.Blocker != nil {
		return res.Blocker.TaskID
	}
	return ""
}
