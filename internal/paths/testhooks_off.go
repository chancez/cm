//go:build !cm_testhooks

package paths

import "time"

// Test hooks, absent from a released build.
//
// This file and testhooks_on.go are the two halves of the cm_testhooks tag. Gating on a build tag rather than
// on an environment variable alone is the point: an env var a released binary honors is one a stale `export`
// in a shell profile can quietly change behavior with, and CM_VERSION specifically is one someone could set
// on both sides to silence the skew warning that exists to catch a real mismatch. Neither is possible if the
// code that reads it is not in the binary.
//
// One tag for all of them rather than one per hook, so there is a single instrumented binary to build and
// reason about instead of a matrix of them.
//
// Each returns a zero value and inlines to nothing, so a released build pays no branch.

// versionOverride reports no override, because this is a normal build.
func versionOverride() string { return "" }

// SocketWatchIntervalOverride reports no override, because this is a normal build.
//
// The interval it would set is how often a server checks that its own socket path still refers to it. A test
// needs that to be short: the condition is created deliberately and then waited on, and a minute's wait per
// assertion is not a test anyone runs.
func SocketWatchIntervalOverride() (time.Duration, bool) { return 0, false }
