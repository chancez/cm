package server

import (
	"path/filepath"

	"golang.org/x/sys/unix"
)

// ptyUsage reports how many ptys exist and the system limit.
//
// Read from the kernel rather than counted from cm's own sessions, because the limit is system-wide and the
// thing that breaks is usually not cm. A leak of one pty per session shows up as some other program failing
// to open a terminal, and the number that matters is the total.
//
// The count comes from /dev/ttys*, which the kernel creates on allocation and removes on release. Verified
// rather than assumed: opening 20 ptys moved the count from 66 to 86 and closing them returned it to 66.
func ptyUsage() (used, limit int, ok bool) {
	max, err := unix.SysctlUint32("kern.tty.ptmx_max")
	if err != nil {
		return 0, 0, false
	}
	// Glob rather than ReadDir: /dev holds a great many entries and only the ttys ones are wanted.
	matches, err := filepath.Glob("/dev/ttys*")
	if err != nil {
		return 0, 0, false
	}
	return len(matches), int(max), true
}
