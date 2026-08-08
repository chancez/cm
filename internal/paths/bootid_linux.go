package paths

import (
	"os"
	"strings"
)

// BootID identifies the current boot, for distinguishing pids that have been reused.
//
// Linux exposes a real boot uuid, which is better than a timestamp: it needs no encoding and cannot collide.
// Truncated to its first block, since the whole point is telling two boots apart in a log line rather than
// being globally unique, and a full uuid in every line is noise.
//
// Empty when it cannot be read, which happens under a restricted /proc. A caller treats that as "unknown"
// rather than failing.
func BootID() string {
	b, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return ""
	}
	id := strings.TrimSpace(string(b))
	if first, _, ok := strings.Cut(id, "-"); ok {
		return first
	}
	return id
}
