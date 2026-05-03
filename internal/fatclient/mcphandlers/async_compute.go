package mcphandlers

// Async compute kickoff. When a compute task is declared `mode:
// async`, enju_execute_task forks an `enju wrap-task` subprocess
// detached from the MCP session and returns immediately. The
// subprocess runs the user's script, commits with Enju trailers,
// and pushes to origin — on its own timeline, independent of
// whether the user closes their laptop or the MCP server exits.
//
// Reconciliation happens later via the fetch-path scanner (phase
// 4c): any fat client's next fetch on the task's branch sees the
// trailer-tagged commit and POSTs /tasks/reconcile. The
// coordinator advances the task from running (phase 4a) or
// claimed to accepted / failed based on the trailer's exit code.
//
// This file intentionally stays small — the complex work (script
// + commit + push) lives in internal/compute, and this file's
// only job is the fork-and-forget handoff + the corresponding
// reply formatting the user sees.

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"

	"github.com/enju-ai/enju/internal/fatclient/compute"
	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// resolvedMode returns the effective execution mode for a compute
// task from its meta record, applying the default-to-sync rule.
// Non-compute tasks return "" — callers check `action == "compute"`
// before branching on mode.
func resolvedMode(meta *taskMeta) string {
	if meta == nil {
		return ""
	}
	return enjuYaml.ResolvedModeFields(meta.Action, meta.Mode)
}

// asyncKickoffResult captures the launch outcome so the caller
// can tell the user exactly what was spawned and where its logs
// go if they want to tail it manually.
type asyncKickoffResult struct {
	PID         int    // child process PID
	SpecPath    string // temp spec file
	OutputPath  string // temp result file (populated by wrapper on exit)
	WrapperLog  string // wrapper's stderr log file
}

// kickoffAsyncWrapTask spawns a detached `enju wrap-task`
// subprocess and returns without waiting. The subprocess
// inherits `env` (so ENJU_PARAM_* + task env vars reach the
// script). Its stdin is /dev/null, stdout/stderr redirect to a
// log file alongside the task's result dir so post-mortem
// debugging works even when the MCP session is long gone.
//
// Detach mechanism: Setsid puts the child in a new session /
// process group / no controlling terminal, so (a) SIGHUP from
// the user's shell doesn't propagate, and (b) when the parent
// MCP process exits, the child is adopted by init rather than
// killed. The parent doesn't Wait() — a background goroutine
// reaps to avoid a zombie, but only while the MCP process is
// alive; init handles orphan cleanup after parent exit.
func (c *apiClient) kickoffAsyncWrapTask(spec compute.Spec, env []string, resultDir, workDir string) (*asyncKickoffResult, error) {
	self, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("locating enju binary: %w", err)
	}

	// Temp spec + result files. Live under the run's result dir
	// (not /tmp) so users can inspect them the same way they'd
	// inspect other task artifacts. The wrapper deletes nothing;
	// leftovers after completion are fine — they're alongside
	// the committed result.
	runSubdir := filepath.Join(workDir, resultDir)
	if err := os.MkdirAll(runSubdir, 0755); err != nil {
		return nil, fmt.Errorf("creating run dir: %w", err)
	}
	specPath := filepath.Join(runSubdir, ".wrap-spec.json")
	outputPath := filepath.Join(runSubdir, ".wrap-result.json")
	wrapperLogPath := filepath.Join(runSubdir, "wrapper.log")

	specBytes, err := json.MarshalIndent(spec, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding spec: %w", err)
	}
	if err := os.WriteFile(specPath, specBytes, 0600); err != nil {
		return nil, fmt.Errorf("writing spec: %w", err)
	}

	logFile, err := os.Create(wrapperLogPath)
	if err != nil {
		return nil, fmt.Errorf("opening wrapper log: %w", err)
	}
	// Note: we do NOT defer logFile.Close(). The subprocess
	// inherits the fd; closing here would yank it mid-write.
	// The OS closes it when the subprocess exits.

	cmd := exec.Command(self, "wrap-task", "--spec", specPath, "--output", outputPath)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	// Setsid: new session, new process group, detached from any
	// controlling terminal. Key invariant for "outlives the MCP
	// session."
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return nil, fmt.Errorf("starting wrap-task: %w", err)
	}
	pid := cmd.Process.Pid

	// Reap in background so we don't leave a zombie if the MCP
	// process stays up after the child completes. If the MCP
	// process exits first, init adopts + reaps the child —
	// either way no leak.
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	return &asyncKickoffResult{
		PID:        pid,
		SpecPath:   specPath,
		OutputPath: outputPath,
		WrapperLog: wrapperLogPath,
	}, nil
}

// formatAsyncKickoff renders the user-facing text returned by
// enju_execute_task when the task was launched asynchronously.
// Surfaces exactly enough to let the user track progress manually
// (the wrapper's log file path) without suggesting the task is
// already done — it explicitly says "running in the background."
func formatAsyncKickoff(taskID, scriptLabel string, res *asyncKickoffResult) string {
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
