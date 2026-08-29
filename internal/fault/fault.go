// Package fault is where a test can make cm slow, stall, or fail at a named place.
//
// It exists because most of cm's expensive bugs are timing, and the windows are too narrow to hit on
// purpose. `Session.resumePoints` documents one that is a few instructions wide; the bug where a read-only
// follower revived the session it was watching was found only by race-instrumenting the binary, which
// happened to slow a client enough to lose a startup race; and the partial-sequence bug that lost nine
// bytes of an SGR reproduced about one attach in eight. Each of those is deterministic if the window can
// be widened from outside.
//
// The alternative, and what this replaces, is a callback field on whatever struct needs one, set by a test
// and nil in production. That works and does not scale: the fields accumulate on types that have nothing
// to do with testing, every one is a nil check in a hot path, and there is no list anywhere of what can be
// intervened in. Here there is one list, in points.go.
//
// # Shape of a call site
//
// The whole cost in production is a call to an empty function, which inlines away:
//
//	fault.At(fault.AfterLogAppend)
//
// And where a fault has to be observable as a failure rather than a delay:
//
//	if err := fault.Err(fault.BeforeShimWrite); err != nil {
//	    return err
//	}
//
// No struct field, no nil check, no wiring. A released binary contains neither the behavior nor the
// parsing: see fault_off.go.
//
// # Adding a point
//
// One constant in points.go and one call. That is deliberately the whole procedure, because a mechanism
// that is awkward to extend gets bypassed with a one-off callback, which is what this exists to stop.
//
// # Adding a fault type
//
// One case in the switch in fault_on.go. The types are kept few on purpose: a delay widens a window, a
// pause synchronizes with a test that has to act inside one, and an error exercises a path that is
// otherwise unreachable. Anything more specific belongs in the test rather than in a spec string.
package fault

// Point is a named place in cm's execution where a test can intervene.
//
// A string rather than an int, so the environment can name one and a mismatch is reportable rather than
// silently landing on a different point.
type Point string

// Spec is the environment variable, without the cm prefix, that configures faults.
//
// The value is a comma-separated list of `point:type[=arg][:count=n]`, for example:
//
//	after-log-append:delay=50ms
//	before-model-feed:pause=/tmp/t/go,after-log-append:error:count=1
//
// Unknown points and types are reported rather than ignored, since a fault that silently does nothing
// makes a test pass for the wrong reason, which is the failure mode this whole package is meant to expose.
const SpecEnvSuffix = "TESTHOOK_FAULTS"
