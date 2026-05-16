package webui

import (
	"net/http"
	"strconv"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// Compute task execution surface — both per-task and bulk-per-
// run, mirroring enju_execute_task / enju_execute_run.
//
//   POST /p/{projectID}/t/{taskID}/execute  — run one compute task
//   POST /p/{projectID}/r/{runSeq}/execute  — drain ready compute
//                                              tasks in this run
//
// Both go through Session.ExecuteComputeTask / ExecuteRun which
// own the workspace lock + reconcile + claim + execute + report
// cascade. CSRF-gated by the same-origin middleware.

// handleExecuteComputeTask is POST /p/{pid}/t/{tid}/execute.
// Runs the task end-to-end (claim if needed, execute script,
// report result). Re-renders the task page after.
//
// Failure modes that surface as banner messages:
//
//   - Go error from ExecuteComputeTask = "cannot even attempt"
//     conditions (no workspace, missing script, wrong action,
//     claim gate closed). Banner with the error text.
//   - Outcome.Status == "failed" / "git_failed" = the script
//     ran but exited non-zero, or the post-script git commit
//     failed. ExecuteComputeTask returns nil error in this
//     case — we have to inspect Status. Banner with exit
//     code + first stderr lines + script log path.
//   - Outcome.Status == "async_started" / "completed" = success
//     path; just re-render the task page.
func (s *Server) handleExecuteComputeTask(w http.ResponseWriter, r *http.Request) {
	pid, taskID, ok := parseTaskRoute(w, r)
	if !ok {
		return
	}
	outcome, err := s.fc.ExecuteComputeTask(r.Context(), taskID)
	if err != nil {
		s.logger.Error("ExecuteComputeTask failed", "task_id", taskID, "error", err)
		s.renderTaskActionError(w, r, pid, taskID, "execute failed: "+err.Error())
		return
	}
	if outcome != nil && (outcome.Status == "failed" || outcome.Status == "git_failed") {
		s.logger.Warn("compute task exited non-zero",
			"task_id", taskID,
			"status", outcome.Status,
			"exit_code", outcome.ExitCode,
			"script_log", outcome.ScriptLogPath)
		s.renderTaskActionError(w, r, pid, taskID, formatExecuteFailure(outcome))
		return
	}
	s.renderTaskAfterAction(w, r, pid, taskID)
}

// formatExecuteFailure builds the banner copy for a failed
// compute outcome. Includes exit code, ErrorMessage if present,
// the script's stderr (capped so a 10MB log doesn't overrun
// the banner), and a pointer at the on-disk log file for the
// user to tail if they want more.
func formatExecuteFailure(o *service.ExecuteOutcome) string {
	parts := []string{
		"script " + o.Status + " (exit code " + itoa(o.ExitCode) + ")",
	}
	if o.ErrorMessage != "" {
		parts = append(parts, "error: "+o.ErrorMessage)
	}
	if o.Stderr != "" {
		stderr := o.Stderr
		const max = 800
		if len(stderr) > max {
			stderr = stderr[:max] + " …(truncated)"
		}
		parts = append(parts, "stderr:\n"+stderr)
	}
	if o.ScriptLogPath != "" {
		parts = append(parts, "full log: "+o.ScriptLogPath)
	}
	return strings_join(parts, "\n\n")
}

// strings_join is a tiny helper to keep the imports clean
// (avoids dragging strings into this file just for one Join).
func strings_join(parts []string, sep string) string {
	out := ""
	for i, p := range parts {
		if i > 0 {
			out += sep
		}
		out += p
	}
	return out
}

// itoa is the trivial int → string we already need to avoid
// importing strconv just for one ExitCode → string call. Same
// reasoning as strings_join above. (We DO import strconv at
// the top of this file — could use strconv.Itoa directly, but
// keeping the helper makes the formatExecuteFailure body easier
// to read.)
func itoa(n int) string { return strconv.Itoa(n) }

// handleExecuteRun is POST /p/{pid}/r/{seq}/execute. Drains
// every ready compute task in the run via Session.ExecuteRun.
// Optional form fields:
//
//   max_tasks — cap (default unlimited)
//   parallel  — concurrency (default 1)
//
// Blocks until the cascade completes (a stop reason fires:
// idle, max-tasks, blocker, error). Long runs block the
// request — fine for v1, later we can add an async-progress
// page if it bites.
func (s *Server) handleExecuteRun(w http.ResponseWriter, r *http.Request) {
	pid, seq, ok := parseRunRoute(w, r)
	if !ok {
		return
	}
	_ = r.ParseForm()
	max := 0
	if raw := r.FormValue("max_tasks"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			max = n
		}
	}
	parallel := 1
	if raw := r.FormValue("parallel"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			parallel = n
		}
	}
	_, err := s.fc.ExecuteRun(r.Context(), service.ExecuteRunParams{
		ProjectID: int(pid),
		RunSeq:    seq,
		MaxTasks:  max,
		Parallel:  parallel,
	})
	if err != nil {
		s.logger.Error("ExecuteRun failed", "project_id", pid, "run_seq", seq, "error", err)
		s.renderActionError(w, r, "execute_run failed: "+err.Error())
		return
	}
	s.renderRunAfterAction(w, r, pid, seq)
}
