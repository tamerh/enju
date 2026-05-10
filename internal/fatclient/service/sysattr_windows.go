//go:build windows

package service

import "syscall"

// detachSysProcAttr on Windows returns CREATE_NEW_PROCESS_GROUP so
// Ctrl-C from the parent console doesn't propagate to the child.
// Windows has no Setsid equivalent; this is the closest analogue.
func detachSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{CreationFlags: 0x00000200} // CREATE_NEW_PROCESS_GROUP
}
