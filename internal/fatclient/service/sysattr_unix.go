//go:build !windows

package service

import "syscall"

// detachSysProcAttr returns a SysProcAttr that puts the child in a
// new session (Setsid), detaching it from the parent's controlling
// terminal so SIGHUP / SIGTERM from a closing shell don't propagate.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
