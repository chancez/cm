package paths

import (
	"strconv"

	"golang.org/x/sys/unix"
)

// BootID identifies the current boot, for distinguishing pids that have been reused.
//
// The boot time in seconds, base36-encoded to keep it short. A time rather than a uuid because darwin has no
// boot uuid to read; two boots a second apart would collide, which no machine does.
//
// Empty when it cannot be read. A caller treats that as "unknown" rather than failing: this exists to make a
// log line easier to attribute, and not knowing the boot is better than not logging.
func BootID() string {
	tv, err := unix.SysctlTimeval("kern.boottime")
	if err != nil {
		return ""
	}
	return strconv.FormatInt(int64(tv.Sec), 36)
}
