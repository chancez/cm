//go:build darwin

package graphics

import (
	"fmt"
	"os"
	"testing"
	"unsafe"

	"golang.org/x/sys/unix"
)

// writeShmObject creates a POSIX shared memory object holding data, and returns its name.
//
// Created through shm_open rather than as a file, because darwin exposes no filesystem path for these.
// That asymmetry is the reason the production code has two implementations, so a test that faked one with
// a temp file would exercise the wrong path on this platform.
//
// The name is kept short: darwin caps it at 30 bytes, which is why a real one from icat looks like
// "icat-H2FTCFZLBQCF6" rather than anything path-shaped.
func writeShmObject(t *testing.T, data []byte) string {
	t.Helper()

	name := fmt.Sprintf("/cm-t-%d", os.Getpid())
	bname, err := unix.BytePtrFromString(name)
	if err != nil {
		t.Fatalf("BytePtrFromString() error = %v", err)
	}

	// Unlinked first, so a previous run that died before cleaning up cannot make this one fail with EEXIST
	// and look like a bug in the code under test.
	unix.Syscall(unix.SYS_SHM_UNLINK, uintptr(unsafe.Pointer(bname)), 0, 0)

	fd, _, errno := unix.Syscall(unix.SYS_SHM_OPEN,
		uintptr(unsafe.Pointer(bname)),
		uintptr(os.O_CREATE|os.O_EXCL|os.O_RDWR),
		uintptr(0o600))
	if errno != 0 {
		t.Fatalf("shm_open(%q) error = %v", name, errno)
	}
	f := os.NewFile(fd, name)
	t.Cleanup(func() {
		f.Close()
		unix.Syscall(unix.SYS_SHM_UNLINK, uintptr(unsafe.Pointer(bname)), 0, 0)
	})

	if err := f.Truncate(int64(len(data))); err != nil {
		t.Fatalf("Truncate() error = %v", err)
	}

	// Written through a mapping rather than with WriteAt, because darwin's shared memory descriptors do not
	// support seeking: WriteAt on one fails with "illegal seek". mmap is how a program actually fills one,
	// which is also what icat does.
	m, err := unix.Mmap(int(f.Fd()), 0, len(data), unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		t.Fatalf("Mmap() error = %v", err)
	}
	copy(m, data)
	if err := unix.Munmap(m); err != nil {
		t.Fatalf("Munmap() error = %v", err)
	}
	return name
}
