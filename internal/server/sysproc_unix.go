//go:build unix

package server

import "syscall"

// newShimSysProcAttr detaches a spawned shim from the server's process group.
//
// Setsid gives the shim its own session, so a signal delivered to the server's process
// group, which is what Ctrl-C in a foreground server or a group kill sends, cannot reach
// it. Without this, stopping the server would take every shell with it, defeating the
// point of the shim layer.
func newShimSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
