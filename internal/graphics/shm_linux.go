//go:build linux

package graphics

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// shmDir is where Linux exposes POSIX shared memory objects.
//
// A real directory here, unlike darwin, where the names live in a namespace reachable only through the
// shm_open syscall. That is why this file is short and the darwin one is not, and why assuming either
// shape everywhere is wrong: a path-based reader finds nothing on darwin, and a syscall-based one on
// Linux would work but duplicate what the filesystem already offers.
const shmDir = "/dev/shm"

// openShm opens a POSIX shared memory object by name, read only.
func openShm(name string) (*os.File, error) {
	// A name with a separator in it would escape the directory, so it is refused rather than cleaned:
	// the protocol names an object, and anything path-shaped is not one.
	if strings.ContainsRune(name, filepath.Separator) {
		return nil, fmt.Errorf("%w: shared memory name %q contains a path separator",
			ErrTransferRefused, name)
	}
	f, err := os.Open(filepath.Join(shmDir, name))
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrTransferRefused, err)
	}
	return f, nil
}

// unlinkShm removes a shared memory object, which is what a terminal does after reading one.
func unlinkShm(name string) error {
	if strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("shared memory name %q contains a path separator", name)
	}
	return os.Remove(filepath.Join(shmDir, name))
}

// readShm returns the contents of an open shared memory object.
//
// An ordinary read here, because Linux exposes these as files under /dev/shm and reading one works. The
// darwin implementation has to map instead: a descriptor from shm_open there does not support read(2) at
// all, and calling it fails with ENXIO, which surfaces as "device not configured".
func readShm(f *os.File, size int) ([]byte, error) {
	out := make([]byte, size)
	if _, err := readFull(f, out); err != nil {
		return nil, fmt.Errorf("%w: reading shared memory: %w", ErrTransferRefused, err)
	}
	return out, nil
}
