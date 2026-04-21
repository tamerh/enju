package mcpserver

// Async-wrapper failure reaper. Companion to the fetch-path
// scanner: the scanner catches SUCCESSFUL async completions via
// Enju-Task-Complete commit trailers; the reaper catches
// FAILURES by walking the `.wrap-result.json` files the detached
// wrapper writes alongside each task's result directory.
//
// Why two channels: today's wrapper (matching the sync path)
// doesn't commit on exit != 0 — script.log stays local, no git
// trail. That keeps the git history clean but means a failed
// async task has no scanner-visible signal. The reaper fills
// that gap: when the submitter comes back online (or any future
// tool call touches the project), we sweep result files and
// notify the coordinator of any failures we find.
//
// Trade-off accepted for v1: the submitter must reconnect for
// failures to propagate. Collaborators on other machines won't
// see the failure until then. Rationale: the submitter IS the
// person who kicked off the async task; expecting their client
// to eventually reconnect is reasonable. A future upgrade could
// have the wrapper commit a failure marker (with Enju-Exit: N
// trailer) so the scanner picks failures up too.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/enju-ai/enju/internal/compute"
	"github.com/enju-ai/enju/internal/mcpgit"
)

// reapWrapperFailures walks the project's enju/runs tree
// looking for detached-wrapper result files whose recorded exit
// is non-zero. For each, posts /tasks/:id/fail and moves the
// result file aside so we don't re-notify. Silent on failures —
// a post that errors leaves the file in place so a retry on
// next call sweeps it up.
//
// Called from reconcile hook points; cost is one directory
// walk bounded by the number of runs × instances in the
// project's enju/runs tree. Empty or sync-only projects
// terminate quickly because most directories hold no
// .wrap-result.json file.
func (c *apiClient) reapWrapperFailures(ctx context.Context, proj *mcpgit.Project, projectID int64) {
	if proj == nil {
		return
	}
	root := filepath.Join(proj.WorkDir(), "enju", "runs")
	if _, err := os.Stat(root); os.IsNotExist(err) {
		return
	}

	_ = filepath.WalkDir(root, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil || d == nil || d.IsDir() {
			return nil
		}
		if filepath.Base(path) != ".wrap-result.json" {
			return nil
		}
		c.handleOneWrapperResult(ctx, path)
		return nil
	})
}

// handleOneWrapperResult processes one wrapper result file.
// Reads result + corresponding spec, and when the result
// indicates a script failure posts /tasks/:id/fail to the
// coordinator. On success the result file is renamed to
// .wrap-result.done.json so a later reap doesn't revisit it.
// Idempotent — the coordinator rejects /fail on tasks already
// in terminal state, which we treat as "already handled, move
// on."
func (c *apiClient) handleOneWrapperResult(ctx context.Context, resultPath string) {
	data, err := os.ReadFile(resultPath)
	if err != nil {
		return
	}
	var res compute.Result
	if err := json.Unmarshal(data, &res); err != nil {
		// Malformed file — don't keep retrying forever; rename
		// so the reaper skips it. A human can inspect after.
		_ = os.Rename(resultPath, resultPath+".malformed")
		return
	}
	if res.ExitCode == 0 && res.Error == "" {
		// Success case — the trailer scanner handles this via
		// /tasks/reconcile. We just need to mark the file
		// processed so the next reap walk doesn't slow down
		// re-reading it.
		_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
		return
	}

	// Failure path: read the companion spec file for the task
	// id. Spec lives next to result in the wrapper's kickoff
	// payload (see kickoffAsyncWrapTask).
	specPath := filepath.Join(filepath.Dir(resultPath), ".wrap-spec.json")
	specBytes, err := os.ReadFile(specPath)
	if err != nil {
		// No spec → can't name the task. Skip; a future
		// wrapper version might emit task_id in the result
		// directly to avoid this dependency.
		return
	}
	var spec compute.Spec
	if err := json.Unmarshal(specBytes, &spec); err != nil {
		return
	}
	if spec.TaskID == "" {
		return
	}

	reason := buildFailReason(spec, res)
	_, postErr := c.post(ctx, fmt.Sprintf("/api/v1/tasks/%s/fail", spec.TaskID), map[string]string{
		"reason": reason,
	})
	if postErr != nil {
		// Coordinator-side refusal (already terminal,
		// membership, etc) is fine — treat as "handled,
		// move on." Network errors also leave the file
		// alone; next reap will retry.
		if !strings.Contains(postErr.Error(), "terminal") {
			c.logger.Debug("reap post fail", "task_id", spec.TaskID, "error", postErr)
			return
		}
	}
	_ = os.Rename(resultPath, strings.TrimSuffix(resultPath, ".json")+".done.json")
}

// buildFailReason renders a short human-readable failure
// message from the wrapper's result. Prefers a wrapper-level
// Error (spec parse / fork failures) over script exit code +
// stderr tail, since the former is the truer root cause. Caps
// the stderr tail to keep the coordinator payload reasonable.
func buildFailReason(spec compute.Spec, res compute.Result) string {
	if res.Error != "" {
		return fmt.Sprintf("async wrap-task failed: %s", res.Error)
	}
	label := spec.ScriptLabel
	if label == "" {
		label = "script"
	}
	msg := fmt.Sprintf("async %s exited with code %d", label, res.ExitCode)
	if res.Stderr != "" {
		tail := res.Stderr
		if len(tail) > 800 {
			tail = tail[:800] + "...(truncated)"
		}
		msg += ": " + tail
	}
	return msg
}
