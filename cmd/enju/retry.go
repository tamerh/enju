package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// cmdRetry is `enju retry <task-id>`: re-open and re-run a single
// failed_retryable task. Thin CLI mirror of enju_retry_task — pairs
// with `enju resume` for the recover-an-existing-run loop (retry the
// failed task, then resume drains the rest).
//
// <task-id> is the full project:run:task id printed in failure output.
func cmdRetry(args []string) {
	fs := flag.NewFlagSet("retry", flag.ExitOnError)
	from := fs.String("from", "head", `Which script version to re-run: "head" (re-materialize from the run-branch tip — commit your fix to the run branch first) or "snapshot" (re-run the pinned snapshot unchanged, for a transient failure where the code was never the problem).`)
	coordOverride := fs.String("coordinator", "", "Coordinator URL (default: from credentials.json)")
	asJSON := fs.Bool("json", false, "Emit the retry outcome as a JSON record on stdout")
	fs.Parse(args)

	if fs.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: enju retry <task-id> [--from head|snapshot] [--json]")
		os.Exit(2)
	}
	taskID := fs.Arg(0)
	if err := validateRetryFrom(*from); err != nil {
		fmt.Fprintf(os.Stderr, "--from: %v\n", err)
		os.Exit(2)
	}

	sess := openCLISession(*coordOverride)
	os.Exit(runRetry(context.Background(), sess, taskID, *from, *asJSON))
}

// validateRetryFrom accepts only the two coord-recognized retry modes.
// Extracted so the validation is testable without coord plumbing.
func validateRetryFrom(from string) error {
	switch from {
	case "head", "snapshot":
		return nil
	default:
		return fmt.Errorf("invalid value %q (must be head or snapshot)", from)
	}
}

// runRetry performs the two-half retry compose and returns the process
// exit code. Returns 0 on a successful re-open (+ successful compute
// re-run, if compute), 1 on coord refusal or a failed re-run.
//
// PARITY: this mirrors mcphandlers/task.go handleRetryTask. The
// coordinator re-opens the task (failed_retryable → READY); the
// client-side execute SECOND half only applies to compute tasks — a
// citizen task (answer/review/vote) is re-open-only, and running the
// compute path on it would surface a spurious "not compute" error even
// though the recovery already succeeded. Keep the two sites in sync.
func runRetry(ctx context.Context, sess *cliSession, taskID, from string, asJSON bool) int {
	data, err := sess.FC.Coord().Post(ctx, "/api/v1/tasks/"+taskID+"/retry", map[string]string{"from": from})
	if err != nil {
		fmt.Fprintf(os.Stderr, "retry: %v\n", err)
		return 1
	}
	var resp struct {
		Error     string `json:"error"`
		From      string `json:"from"`
		IsCompute bool   `json:"is_compute"`
	}
	_ = json.Unmarshal(data, &resp)
	if resp.Error != "" {
		fmt.Fprintf(os.Stderr, "retry: %s\n", resp.Error)
		return 1
	}
	if resp.From != "" {
		from = resp.From // coordinator-normalized intent
	}

	if !resp.IsCompute {
		// Citizen task: re-open only. Its assignee re-claims and
		// re-runs; there is no operator-driven execute step.
		fmt.Printf("↻ retried %s — re-opened to READY (from=%s). Its assignee re-claims and re-runs; PENDING descendants auto-promote once it delivers.\n", taskID, from)
		return 0
	}

	outcome, err := sess.FC.RetryComputeTask(ctx, taskID, from)
	if err != nil {
		fmt.Fprintf(os.Stderr, "retry: %v\n", err)
		return 1
	}
	return renderRetryOutcome(outcome, from, asJSON)
}

// renderRetryOutcome prints a single compute-retry outcome and returns
// the exit code (1 on failure/error, 0 on success or async launch).
func renderRetryOutcome(out *service.ExecuteOutcome, from string, asJSON bool) int {
	if asJSON {
		rec := map[string]any{
			"type":       "retry",
			"task_id":    out.TaskID,
			"status":     out.Status,
			"from":       from,
			"commit_sha": out.CommitSHA,
			"elapsed_ms": out.ElapsedMS,
			"reason":     retryFailReason(out),
		}
		b, _ := json.Marshal(rec)
		fmt.Println(string(b))
	}
	switch service.ClassifyEntryStatus(out.Status) {
	case service.EntryClassSuccess:
		if !asJSON {
			fmt.Printf("↻ %s re-ran OK (from=%s, %dms)\n", out.TaskID, from, out.ElapsedMS)
		}
		return 0
	case service.EntryClassPending:
		if !asJSON {
			fmt.Printf("↻ %s re-launched (async, from=%s)\n", out.TaskID, from)
		}
		return 0
	default: // Failed, GitFailed, Error, Unknown, Skipped
		if !asJSON {
			fmt.Printf("✗ %s retry failed — %s\n", out.TaskID, retryFailReason(out))
		}
		return 1
	}
}

// retryFailReason picks the most informative failure string from a
// compute outcome: the wrapper error, else the stderr tail.
func retryFailReason(out *service.ExecuteOutcome) string {
	if out.ErrorMessage != "" {
		return out.ErrorMessage
	}
	return out.Stderr
}
