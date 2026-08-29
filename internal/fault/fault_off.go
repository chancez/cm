//go:build !cm_testhooks

package fault

// The production half: every entry point is empty, so a released binary contains no fault machinery and
// no way to name one from the environment.
//
// This is the reason the package is shaped around two free functions rather than an injected interface.
// An interface would need a nil check at each site and a field on whatever holds it, and the check would
// survive into the release build. These inline to nothing, so the cost of a point is a line of source and
// zero instructions.
//
// It also settles what a fault can be. Anything a call site would have to branch on, or thread a value
// through, cannot be expressed here without the release build paying for it, which is the constraint that
// keeps the set of fault types small on purpose. See fault_on.go.

// At is a fault point. Does nothing in a released build.
func At(Point) {}

// Err is a fault point that can fail. Always nil in a released build.
//
// Separate from At because the call sites differ: a delay is invisible to the code around it, while an
// error has to be returned, and pretending otherwise would mean every point looked like it could fail.
func Err(Point) error { return nil }

// Enabled reports whether any fault is configured. Always false in a released build.
//
// For the rare site that would have to do real work to reach a point at all. Not for skipping the call to
// At, which is already free.
func Enabled() bool { return false }
