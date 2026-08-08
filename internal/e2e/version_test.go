package e2e

import (
	"slices"
	"strings"
	"testing"
	"time"
)

// newVersionEnv returns an isolated installation whose binary honors CM_VERSION.
//
// Separate from newEnv because the override is behind a build tag on purpose: a released cm ignores
// CM_VERSION entirely, so a test that wants two disagreeing versions has to ask for an instrumented build.
//
// The version is set before the first command, which is what starts the server. That ordering is
// load-bearing: newEnv starts a server eagerly to avoid a startup race, so a version set afterwards would
// apply only to later clients and the server would still report the real build.
func newVersionEnv(t *testing.T, version string) *env {
	t.Helper()

	e := newEnvWith(t, cmVersionBinary(t), version)
	// Confirm the override actually took effect, rather than trusting the build tag. Without this the tests
	// below would silently compare the real version against itself and pass.
	if got, _ := e.doctor(); got.ServerVersion != version {
		t.Fatalf("server reported %q, want the override %q: the instrumented build is not in use",
			got.ServerVersion, version)
	}
	return e
}

// A client and server from different builds is reported as skew, across a real process boundary.
//
// This is the part that cannot be tested honestly in one process. version-skew exists because a client and a
// server are separate programs with separate builds, and the behavior worth checking is that each reports its
// own version: a unit test can drive the comparison, which it does, but it cannot catch a server that echoes
// back whatever the client told it.
func TestDoctorReportsVersionSkewBetweenProcesses(t *testing.T) {
	skipIfShort(t)

	e := newVersionEnv(t, "v1.0.0")

	// A matching client first, so the test establishes that skew is not reported unconditionally. Without
	// this half, a check hardcoded to always warn would pass the interesting half below.
	same, _ := e.doctor()
	if slices.Contains(same.kinds(), "version-skew") {
		t.Fatalf("version-skew reported for matching versions: %v", same.kinds())
	}

	// Now a client claiming a different build. Only the client changes; the server is the same process,
	// still reporting v1.0.0, which is what makes this genuine skew rather than two values from one place.
	e.version = "v2.0.0"
	res, code := e.doctor()

	if res.ClientVersion != "v2.0.0" {
		t.Errorf("client_version = %q, want v2.0.0: the client must send its own build", res.ClientVersion)
	}
	if res.ServerVersion != "v1.0.0" {
		t.Errorf("server_version = %q, want v1.0.0: the server must answer with its own build rather than "+
			"the client's", res.ServerVersion)
	}

	skew := res.ofKind("version-skew")
	if len(skew) != 1 {
		t.Fatalf("findings = %v, want one version-skew", res.kinds())
	}
	// Both versions appear in the message, since knowing they differ without knowing which side is old does
	// not tell the reader what to do.
	for _, want := range []string{"v1.0.0", "v2.0.0"} {
		if !strings.Contains(skew[0].Detail, want) {
			t.Errorf("detail does not name %s: %q", want, skew[0].Detail)
		}
	}
	// Not fixable: restarting the server is the user's call, and doing it from a diagnostic would drop
	// every client attached at the time.
	if skew[0].Fixable {
		t.Error("Fixable = true, want false: a diagnostic must not restart the server behind the user")
	}

	// A finding means a non-zero exit. Skew is a warning in the sense that cm keeps working, and it still
	// has to be visible to a script checking the status.
	if code == 0 {
		t.Error("exit code = 0 with a finding, want non-zero so this can gate a script")
	}
}

// A restarted server reports its own version, not the one that created the session.
//
// The upgrade case, and the one cm is built for: a session outlives the server that made it, so a new build
// adopts sessions an old one created. The version reported has to be the running server's, or the check
// compares against a process that no longer exists.
func TestServerVersionFollowsTheRunningServer(t *testing.T) {
	skipIfShort(t)

	e := newVersionEnv(t, "v1.0.0")

	// A session created by the old server, so the restart has something to adopt and this covers the real
	// upgrade path rather than a bare restart.
	e.mustRun("run", "--session", "carried", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the session to be running", 15*time.Second, func() bool {
		s, ok := e.session("carried")
		return ok && s.State == "running"
	})
	before, _ := e.session("carried")

	// Replace the server with one claiming a newer build, the way an upgrade does. The version is changed
	// while no server is running, so the next command starts one that reports the new value.
	e.mustRun("server", "stop")
	e.waitServerGone()
	e.version = "v1.1.0"
	e.mustRun("list")

	res, _ := e.doctor()
	if res.ServerVersion != "v1.1.0" {
		t.Errorf("server_version after the restart = %q, want v1.1.0: the reported version must be the "+
			"running server's, not that of the one which created the session", res.ServerVersion)
	}
	// Client and server agree again, so no skew: both are the same instrumented binary reading the same
	// override. A check that compared against a remembered value rather than the live server would still
	// report skew here.
	if slices.Contains(res.kinds(), "version-skew") {
		t.Errorf("findings = %v, want no skew once both sides report v1.1.0", res.kinds())
	}

	// And the session really was carried across, which is what makes this an upgrade. Without this the test
	// would pass for a server that started fresh and dropped everything.
	after, ok := e.session("carried")
	if !ok {
		t.Fatal("the session was not adopted by the new server")
	}
	if after.ShellPID != before.ShellPID {
		t.Errorf("shell pid changed: %d -> %d, want the same process adopted",
			before.ShellPID, after.ShellPID)
	}
}

// A released binary ignores CM_VERSION.
//
// The guard on the whole mechanism, and the reason it is behind a build tag rather than only an environment
// variable. An override a released cm honored would let a stale `export CM_VERSION` in a shell profile make
// every version report a lie, and would let someone mute the skew warning by setting both sides to match,
// which defeats the check it exists to support. This asserts the gate instead of trusting it.
func TestReleasedBinaryIgnoresTheVersionOverride(t *testing.T) {
	skipIfShort(t)

	// A normal env, so the binary is the ordinary one rather than the instrumented build.
	e := newEnv(t)
	e.version = "v9.9.9-should-be-ignored"

	res, _ := e.doctor()
	if res.ClientVersion == e.version || res.ServerVersion == e.version {
		t.Fatalf("a released build honored CM_VERSION: client %q server %q",
			res.ClientVersion, res.ServerVersion)
	}
	// Neither side is empty either, since a build that returned "" would also not equal the override and
	// would pass the check above while breaking every version report.
	if res.ClientVersion == "" || res.ServerVersion == "" {
		t.Errorf("versions = client %q server %q, want both to name a build",
			res.ClientVersion, res.ServerVersion)
	}
	// And no skew, since both sides fell back to the same real build.
	if slices.Contains(res.kinds(), "version-skew") {
		t.Errorf("findings = %v, want no version-skew when both sides report the real build", res.kinds())
	}
}
