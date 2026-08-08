//go:build !darwin && !linux

package server

// ptyUsage reports that pty accounting is unavailable.
//
// A stub rather than a build failure, so cm compiles on a platform nobody has taught it about yet. The
// consequence is one check that never fires, which is the right trade: the alternative is guessing at a
// limit, and a diagnostic that invents numbers is worse than one that stays quiet.
func ptyUsage() (used, limit int, ok bool) {
	return 0, 0, false
}
