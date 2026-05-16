//go:build !windows

package executor

import "syscall"

// detachSysProcAttr puts the child in a new session (Setsid),
// detaching it from the parent's controlling terminal so SIGHUP
// / SIGTERM from a closing shell (or the MCP session exiting)
// don't propagate. Moved verbatim from the service package's
// kickoffAsyncWrapTask path — same detach contract.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}

// processAlive reports whether pid is still a live process.
// signal 0 performs error-checking without delivering a signal:
// nil ⇒ alive, ESRCH ⇒ gone, EPERM ⇒ alive but not ours (still
// "running" for liveness purposes). Used by LocalExecutor.Poll
// — which only answers "is it still going?", never reads
// .wrap-result.json.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// killProcessTree terminates a detached wrapper. Submit used
// Setsid, so the child leads its own process group; signalling
// the negative pid hits the whole group (the wrapper plus any
// docker/apptainer/script descendants it spawned). SIGTERM
// first for a clean shutdown, then SIGKILL as the backstop.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(-pid, syscall.SIGTERM); err != nil {
		// Fall back to the single process if it isn't a group
		// leader (e.g. a legacy non-Setsid wrapper).
		_ = syscall.Kill(pid, syscall.SIGTERM)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	_ = syscall.Kill(pid, syscall.SIGKILL)
	return nil
}
