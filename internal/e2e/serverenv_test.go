package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A session must not inherit the server's environment, and the values that matter most are the ones a
// client would otherwise have overwritten.
//
// The bug this exists for: a server started from a shell inside an SSH session held SSH_CLIENT,
// SSH_CONNECTION and SSH_TTY. spawnShim layers the creating client's environment over the server's own,
// and a local client has no SSH_CLIENT to overwrite with, so the server's copy reached every session's
// shell. Prompts printed user@host as though remote, in sessions and splits that had never been near
// SSH, and it survived server restarts and reinstalling the previous binary, because the running process
// is what carried it.
//
// Three processes are needed to see it at all: the leak is between the server's environment and a shell
// two spawns away, and a unit test on the composing function cannot show that the server contributes to
// it. newEnvWithServerEnv gives the server values the clients do not have, which is the only way to tell
// inheritance from the client's own environment.
//
// Values are matched rather than names, and are ones that could not come from anywhere else: the test
// machine may legitimately have SSH_AUTH_SOCK, so asserting no variable starts with SSH_ would fail for
// a reason that is not this bug.
func TestSessionDoesNotInheritTheServersClientValues(t *testing.T) {
	skipIfShort(t)

	const (
		fakeClient = "203.0.113.9 51174 22"
		fakeTTY    = "/dev/ttys999"
		fakeTerm   = "ansi-from-the-server"
	)
	e := newEnvWithServerEnv(t,
		"SSH_CLIENT="+fakeClient,
		"SSH_CONNECTION="+fakeClient,
		"SSH_TTY="+fakeTTY,
		// TERM as well, since it is the same leak and the one every session has: a server's TERM would
		// otherwise decide what a shell believes its terminal is.
		"TERM="+fakeTerm,
	)

	dump := filepath.Join(e.state, "session-env")
	e.mustRun("run", "--session", "probe", "-d", "--", "/bin/sh", "-c", "env > "+dump)

	// The control, and it is load-bearing: without it an unwritten or empty dump passes every assertion
	// below while proving nothing. CM_SESSION is set by the shim for the session it serves, so its
	// presence means this really is the session's environment.
	e.waitFor("the session to write its environment", 15*time.Second, func() bool {
		return strings.Contains(e.readFileOrEmpty(dump), "CM_SESSION=probe")
	})
	body := e.readFileOrEmpty(dump)

	for _, leaked := range []string{fakeClient, fakeTTY, fakeTerm} {
		if strings.Contains(body, leaked) {
			t.Errorf("session environment contains %q, which only the server had:\n%s", leaked, body)
		}
	}
}
