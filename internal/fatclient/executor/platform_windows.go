//go:build windows

package executor

import (
	"os"
	"syscall"
)

// detachSysProcAttr on Windows returns CREATE_NEW_PROCESS_GROUP
// so Ctrl-C from the parent console doesn't propagate to the
// child. Windows has no Setsid equivalent; this is the closest
// analogue. Moved verbatim from the service package.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}

// processAlive — best effort on Windows. os.FindProcess always
// succeeds, so probe with Signal(syscall.Signal(0)): a live
// process returns nil, a dead one an error. SLURM is Linux-only
// and the cluster path never runs here; this keeps LocalExecutor
// compiling and roughly correct on Windows dev machines.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	return p.Signal(syscall.Signal(0)) == nil
}

// killProcessTree terminates the wrapper. Windows has no process
// groups in the POSIX sense; kill the process directly.
func killProcessTree(pid int) error {
	if pid <= 0 {
		return nil
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return nil
	}
	return p.Kill()
}
