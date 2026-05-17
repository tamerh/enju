//go:build !windows

package main

import (
	"os/exec"
	"syscall"
)

// setDetachedProcess puts the child in its own process group so it
// doesn't receive SIGHUP or SIGINT directed at the parent terminal.
func setDetachedProcess(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}
