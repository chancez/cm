//go:build darwin

package graphics

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/unix"
)

// shmNameMax is the longest name darwin's shm_open accepts.
//
// 30, which is far shorter than a filesystem path and is why a shared memory name looks nothing like
// one: `kitten icat` sends names such as "icat-H2FTCFZLBQCF6". A name over the limit is refused by the
// kernel with ENAMETOOLONG, and refusing it here first gives a clearer error.
const shmNameMax = 30

// openShm opens a POSIX shared memory object by name, read only.
//
// A syscall rather than an os.Open, because darwin exposes no filesystem path for these: the name lives
// in a separate namespace reached only through shm_open. Linux happens to expose them under /dev/shm,
// which is why that build has a much shorter implementation, and assuming the Linux shape here is what
// makes a naive port read an unrelated file or nothing at all.
func openShm(name string) (*os.File, error) {
	if len(name) > shmNameMax {
		return nil, fmt.Errorf("%w: shared memory name %q is longer than %d bytes",
			ErrTransferRefused, name, shmNameMax)
	}

	bname, err := unix.BytePtrFromString(name)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransferRefused, err)
	}

	for {
		fd, _, errno := unix.Syscall(unix.SYS_SHM_OPEN,
			uintptr(unsafe.Pointer(bname)), uintptr(os.O_RDONLY), 0)
		if errno == unix.EINTR {
			// Retried rather than failed: a signal arriving mid-call is not the program's fault, and cm's
			// server takes signals for its own reasons.
			continue
		}
		if errno != 0 {
			return nil, fmt.Errorf("%w: shm_open(%q): %w", ErrTransferRefused, name, errno)
		}
		return os.NewFile(fd, name), nil
	}
}

// readShm returns the contents of an open shared memory object.
//
// Through a mapping rather than a read, and that is not a preference: a descriptor from shm_open on darwin
// does not support read(2) at all. Calling it fails with ENXIO, which surfaces as "device not configured"
// and reads like a missing device rather than the wrong syscall. mmap is how a program fills one of these
// and how a terminal consumes one, so it is also what kitty does.
//
// size comes from the caller's Stat rather than being re-derived, since a zero-length mapping is invalid
// and has to be handled before getting here.
func readShm(f *os.File, size int) ([]byte, error) {
	m, err := unix.Mmap(int(f.Fd()), 0, size, unix.PROT_READ, unix.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("%w: mapping shared memory: %w", ErrTransferRefused, err)
	}
	// Copied out before unmapping, so nothing handed back aliases memory that is about to be released.
	out := make([]byte, size)
	copy(out, m)
	if err := unix.Munmap(m); err != nil {
		return nil, fmt.Errorf("%w: unmapping shared memory: %w", ErrTransferRefused, err)
	}
	return out, nil
}

// unlinkShm removes a shared memory object, which is what a terminal does after reading one.
//
// Failure is returned rather than ignored so a caller can log it, but it is not fatal: the image has
// already been read by then, so an object left behind is untidy rather than broken.
func unlinkShm(name string) error {
	bname, err := unix.BytePtrFromString(name)
	if err != nil {
		return err
	}
	for {
		_, _, errno := unix.Syscall(unix.SYS_SHM_UNLINK, uintptr(unsafe.Pointer(bname)), 0, 0)
		if errno == unix.EINTR {
			continue
		}
		if errno != 0 {
			return errno
		}
		return nil
	}
}
