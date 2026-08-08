//go:build linux

package client

import "golang.org/x/sys/unix"

const (
	ioctlGetTermios = unix.TCGETS
	ioctlSetTermios = unix.TCSETS
)

// posixVDisable disables a special control character. On Linux _POSIX_VDISABLE is 0.
const posixVDisable = 0
