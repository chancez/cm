//go:build linux

package graphics

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeShmObject creates a POSIX shared memory object holding data, and returns its name.
//
// A file under /dev/shm, which is where Linux exposes these, unlike darwin where the name is reachable
// only through shm_open. Skips rather than fails when the directory is absent, since a container can be
// built without it and a missing kernel feature is not a defect in cm.
func writeShmObject(t *testing.T, data []byte) string {
	t.Helper()

	if _, err := os.Stat(shmDir); err != nil {
		t.Skipf("%s is unavailable, so shared memory transfers cannot be exercised here: %v", shmDir, err)
	}

	name := fmt.Sprintf("cm-t-%d", os.Getpid())
	path := filepath.Join(shmDir, name)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("WriteFile(%q) error = %v", path, err)
	}
	t.Cleanup(func() { os.Remove(path) })
	return name
}
