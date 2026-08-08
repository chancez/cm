//go:build cm_testhooks

package paths

import (
	"os"
	"time"
)

// Test hooks, compiled only under the cm_testhooks tag.
//
// A released binary has none of this code, so it cannot be told it is a version it is not and cannot have its
// timing changed from the environment. See testhooks_off.go for why that matters.

// versionOverride reports a version set in the environment, for a test build only.
//
// It exists because the behavior worth testing is what happens when a client and a server disagree, and
// without an override the only way to produce a disagreement is to build two binaries from two different tags.
// That is a manual procedure, not a test: it cannot run in CI, it silently tests nothing if the tags are
// missing, and it needs a git operation to set up.
//
// Read on every call rather than cached at init, since a test that sets it after the process starts would
// otherwise see the real version and pass for the wrong reason.
func versionOverride() string { return os.Getenv(Env(VersionEnvSuffix)) }

// SocketWatchIntervalOverride reports a socket-watch interval set in the environment.
//
// A test creates the unreachable condition on purpose and then waits for the server to notice it. The
// production interval is a minute, which is right for a server that may run for weeks and useless for a test:
// it would either sleep a minute per assertion or assert nothing.
//
// An unparseable or non-positive value is ignored rather than treated as an error, since a ticker built from
// zero panics and a test build is not the place to add a new way to crash.
func SocketWatchIntervalOverride() (time.Duration, bool) {
	v := os.Getenv(Env(SocketWatchEnvSuffix))
	if v == "" {
		return 0, false
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}
