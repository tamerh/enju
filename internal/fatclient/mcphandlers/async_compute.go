package mcphandlers

// Async compute kickoff display. The kickoff itself
// (FatClient.kickoffAsyncWrapTask) lives in service.execute.go
// alongside the rest of compute orchestration; this file is
// just the per-handler formatter that renders the spawned
// subprocess's PID and log path back to the MCP caller.

import (
	"fmt"

	"github.com/enju-ai/enju/internal/fatclient/service"
)

// formatAsyncKickoff renders the user-facing text returned by
// enju_execute_task when the task was launched asynchronously.
// Surfaces exactly enough to let the user track progress
// manually (the wrapper's log file path) without suggesting
// the task is already done — it explicitly says "running in
// the background."
func formatAsyncKickoff(taskID, scriptLabel string, res *service.AsyncKickoffResult) string {
	if res != nil && res.Executor == "slurm" {
		return fmt.Sprintf(
			"⏳ Submitted as SLURM job %s\n"+
				"  Task:     %s\n"+
				"  Script:   %s\n"+
				"  Job ID:   %s\n\n"+
				"The job is queued on the cluster, detached from this MCP session. The compute\n"+
				"node produces the result; the next fetch-path scan polls sacct, performs the\n"+
				"commit host-side, and reconciles. Track it with `sacct -j %s` or\n"+
				"enju_run_status — the task stays in running state until the job completes.",
			res.JobID, taskID, scriptLabel, res.JobID, res.JobID,
		)
	}
	return fmt.Sprintf(
		"⏳ Script launched in the background (async mode)\n"+
			"  Task:     %s\n"+
			"  Script:   %s\n"+
			"  PID:      %d\n"+
			"  Log:      %s\n\n"+
			"The task is running detached from this MCP session. It will commit + push when the\n"+
			"script completes, and the next fetch-path scan will reconcile the result. You can\n"+
			"tail the log to watch progress, or check enju_run_status — the task stays in running\n"+
			"state until completion.",
		taskID, scriptLabel, res.PID, res.WrapperLog,
	)
}
