package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"strconv"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// cmdResume is `enju resume <seq>`: drain ready compute tasks on an
// EXISTING run, instead of `enju go` which always forks a NEW run.
// Thin CLI mirror of enju_execute_run — same ExecuteRun primitive,
// same renderer + exit semantics as `enju go`'s execute tail (shared
// via executeRunAndRender). Resolves the project like `enju project`
// (--project id, else the project owning cwd) and takes the run's
// per-project seq (the "#7" the user sees), which every coord run
// endpoint resolves against.
//
// The recovery loop after a stop: `enju go` halts at a failure / async
// launch / citizen gate; the operator fixes what's needed
// (enju retry ...) and `enju resume <seq>` drains the rest — without
// re-spending the already-completed work a fresh `enju go` would redo.
func cmdResume(args []string) {
	fs := flag.NewFlagSet("resume", flag.ExitOnError)
	projectID := fs.Int64("project", 0, "Operate on a specific project id (default: the project that owns the current directory)")
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	parallel := fs.Int("parallel", 1, "Run up to N compute tasks concurrently (default 1 = serial). Capped at 32.")
	maxTasks := fs.Int("max-tasks", 1000, "Cap on compute tasks drained in one call (safety net)")
	keepGoing := fs.Bool("keep-going", true, "On a compute failure, record it and keep draining the rest of the run (default). Fix it and `enju retry <id>`.")
	failFast := fs.Bool("fail-fast", false, "Stop at the first compute failure (overrides --keep-going).")
	asJSON := fs.Bool("json", false, "Stream per-task status as JSONL on stdout")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: enju resume <seq> [--parallel N] [--max-tasks N] [--project id] [--json]")
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
		// Both a bad <seq> and a resolution failure (bad/unregistered
		// --project, no registry, cwd not in a project) are usage
		// errors (exit 2) — they happen before any coord round-trip.
		fmt.Fprintf(os.Stderr, "resume: %v\n", rerr)
		os.Exit(2)
	}

	ctx := context.Background()
	os.Exit(executeRunAndRender(ctx, sess, service.ExecuteRunParams{
		ProjectID: int(projID),
		RunSeq:    seq,
		MaxTasks:  *maxTasks,
		Parallel:  parallelN,
		KeepGoing: *keepGoing && !*failFast, // keep-going default; --fail-fast wins
	}, *asJSON))
}

// resolveResumeTarget validates the <seq> argument and resolves the
// active project, returning (projectID, seq). Split from cmdResume
// (which owns flag parsing + os.Exit + the ExecuteRun call) so the
// parse-then-resolve path is testable without a live coordinator.
// Returns a non-nil error for a non-positive/non-numeric seq or a
// resolution failure; both are usage errors the caller maps to exit 2.
func resolveResumeTarget(sess *cliSession, override int64, seqArg string) (projectID int64, seq int, err error) {
	seq, perr := strconv.Atoi(seqArg)
	if perr != nil || seq <= 0 {
		return 0, 0, fmt.Errorf("<seq> must be a positive run number, got %q", seqArg)
	}
	entry, rerr := resolveActiveProject(sess, override)
	if rerr != nil {
		return 0, 0, rerr
	}
	return entry.ID, seq, nil
}
