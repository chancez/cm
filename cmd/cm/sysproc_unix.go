//go:build unix

package main

import "syscall"

// newDetachedSysProcAttr detaches a spawned process from this one's process group.
//
// Setsid means a signal sent to the client's process group, which is what Ctrl-C sends,
// cannot reach the server it just started. Without it, interrupting the client that
// happened to start the server would take the server, and every session, with it.
func newDetachedSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
