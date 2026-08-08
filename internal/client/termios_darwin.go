//go:build darwin

package client

import "golang.org/x/sys/unix"

// The termios get/set ioctls differ by platform: BSD-derived systems use TIOC*ETA, Linux
// uses TC[GS]ETS.
const (
	ioctlGetTermios = unix.TIOCGETA
	ioctlSetTermios = unix.TIOCSETA
)

// posixVDisable is the value that disables a special control character. _POSIX_VDISABLE is
// 0xff on darwin rather than 0 as on Linux, so writing 0 here would map the character to
// NUL instead of disabling it.
const posixVDisable = 0xff
