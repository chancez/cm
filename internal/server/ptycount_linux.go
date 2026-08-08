package server

import (
	"os"
	"strconv"
	"strings"
)

// ptyUsage reports how many ptys are allocated and the system limit.
//
// Linux keeps both as counters under /proc/sys/kernel/pty, which is simpler than the darwin case: nr is the
// live count and max is the cap, so neither has to be derived from device nodes.
//
// A note on what these mean. The limit applies to the devpts instance, and a container commonly has its own
// with a much smaller max than the host, which is exactly the situation where a leak bites soonest.
func ptyUsage() (used, limit int, ok bool) {
	nr, ok := readIntFile("/proc/sys/kernel/pty/nr")
	if !ok {
		return 0, 0, false
	}
	max, ok := readIntFile("/proc/sys/kernel/pty/max")
	if !ok {
		return 0, 0, false
	}
	return nr, max, true
}

// readIntFile reads a single integer from a proc file.
func readIntFile(path string) (int, bool) {
	b, err := os.ReadFile(path)
	if err != nil {
		// Absent under a restricted /proc, which is a reason to skip the check rather than to report a
		// problem: not knowing the pty count is not itself a fault in the installation.
		return 0, false
	}
	n, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil {
		return 0, false
	}
	return n, true
}
