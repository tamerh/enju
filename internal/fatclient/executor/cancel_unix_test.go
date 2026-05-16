//go:build !windows

package executor

import (
	"os/exec"
	"testing"
	"time"
)

// TestKillProcessTreeReallyKills is the real-behavior test for
// the gap this change closes: pre-seam an in-flight async
// compute job had no persisted handle, so enju_terminate_run
// could only cascade DB state and the local process kept
// running. Cancel → killProcessTree must actually stop it.
//
// Mirrors Submit's detach: the child is started Setsid (its
// own process group), then killProcessTree signals the group.
// We assert the PID is gone shortly after.
func TestKillProcessTreeReallyKills(t *testing.T) {
	cmd := exec.Command("sleep", "60")
	cmd.SysProcAttr = detachSysProcAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("starting sleep: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }() // reap so the kernel frees the PID slot

	if !processAlive(pid) {
		t.Fatalf("sleep pid %d not alive right after Start", pid)
	}
	if err := killProcessTree(pid); err != nil {
		t.Fatalf("killProcessTree: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return // killed — success
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("pid %d still alive 3s after killProcessTree", pid)
}

// TestProcessAliveFalseForBogusPID — liveness is false for an
// impossible pid and for pid<=0 (Poll relies on this to report
// StateDone once the wrapper exits).
func TestProcessAliveFalseForBogusPID(t *testing.T) {
	if processAlive(0) || processAlive(-1) {
		t.Error("processAlive(<=0) must be false")
	}
	// PID 2^31-1 is effectively never allocated.
	if processAlive(2147483646) {
		t.Error("processAlive(huge pid) must be false")
	}
}
