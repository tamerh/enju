//go:build windows

package main

import "os/exec"

func setDetachedProcess(cmd *exec.Cmd) {
	// On Windows, processes are not bound to a terminal session the same
	// way. A DETACHED_PROCESS creation flag would be the equivalent but
	// background services are typically managed via the Service Control
	// Manager. For now this is a no-op — enju start/stop is primarily a
	// local-dev convenience and Windows users can run enju serve / enju ui
	// in separate terminals instead.
}
