package executor

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	enjuYaml "github.com/enju-ai/enju/internal/common/yaml"
)

// LocalExecutor runs the wrapper as a detached process on this
// host — today's behavior, now behind the seam. This is Option
// A: local goes through the Executor interface on every
// ordinary run, so the existing async/retry e2e suite IS the
// regression test that the seam preserves behavior.
type LocalExecutor struct{}

var _ Executor = LocalExecutor{}

// Submit spawns `enju wrap-task --spec <s> --output <o>` detached
// and returns without waiting. Mechanically identical to the old
// kickoffAsyncWrapTask: stdin /dev/null, stdout+stderr to a
// wrapper.log beside the result dir, Setsid so a closing MCP
// session / shell SIGHUP doesn't reap it, no parent Wait() (a
// goroutine reaps to avoid a zombie). Resources is ignored — a
// host fork takes whatever the host has.
//
// The one behavior gain vs. the old path: the PID is persisted
// in the .wrap-job.json sidecar, so enju_terminate_run can now
// actually Cancel an in-flight local compute job.
func (LocalExecutor) Submit(ctx context.Context, specPath, outputPath string, env []string, _ enjuYaml.Resources) (Handle, error) {
	self, err := os.Executable()
	if err != nil {
		return Handle{}, fmt.Errorf("locating enju binary: %w", err)
	}
	resultDir := resultDirOf(outputPath)
	logFile, err := os.Create(filepath.Join(resultDir, "wrapper.log"))
	if err != nil {
		return Handle{}, fmt.Errorf("opening wrapper log: %w", err)
	}
	// Deliberately NOT deferring logFile.Close(): the subprocess
	// inherits the fd; closing here would yank it mid-write. The
	// reaper goroutine below closes it once the child exits.

	cmd := exec.Command(self, "wrap-task", "--spec", specPath, "--output", outputPath)
	cmd.Env = env
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = detachSysProcAttr()

	if err := cmd.Start(); err != nil {
		logFile.Close()
		return Handle{}, fmt.Errorf("starting wrap-task: %w", err)
	}
	pid := cmd.Process.Pid
	go func() {
		_ = cmd.Wait()
		logFile.Close()
	}()

	h := Handle{
		Executor:    KindLocal,
		PID:         pid,
		ResultDir:   resultDir,
		SubmittedAt: time.Now().UTC(),
	}
	if err := writeJobSidecar(h); err != nil {
		// The process is already running; failing the submit here
		// would orphan it untracked. Surface the error so the
		// caller logs it, but the wrapper still completes and the
		// existing .wrap-result.json reaper path still reaps it
		// (sidecar only gates Cancel + slurm discovery).
		return h, fmt.Errorf("wrap-task started (pid %d) but persisting %s failed: %w", pid, JobSidecarName, err)
	}
	return h, nil
}

// Poll answers only "is it still going?" — never reads
// .wrap-result.json (the reaper owns exit interpretation). A
// local fork has no queue, so it's Running until the process
// exits, then Done. The reaper then inspects .wrap-result.json;
// its absence after StateDone is what the reaper maps to "lost".
func (LocalExecutor) Poll(ctx context.Context, h Handle) (Status, error) {
	if processAlive(h.PID) {
		return Status{State: StateRunning}, nil
	}
	return Status{State: StateDone}, nil
}

// Cancel kills the persisted process (group). This is the gap
// closer: pre-seam an async wrapper had no persisted handle, so
// enju_terminate_run could only cascade coordinator state and
// the local job kept running. Now the PID is in the sidecar and
// terminate can actually stop it.
func (LocalExecutor) Cancel(ctx context.Context, h Handle) error {
	return killProcessTree(h.PID)
}
