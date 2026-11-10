package e2e

import (
	"testing"
	"time"
)

// --from names the session to act on, so it needs no CM_SESSION. It used to be read only after CM_SESSION
// had been resolved, which failed with "no session given" in any shell outside a session: exactly where a
// script or a keybinding runs `cm switch work --from build`.
//
// Nothing is attached, so the server answers that no window moved. That reply is the evidence: it means
// the request reached the server naming the session --from gave.
func TestSwitchFromNeedsNoSurroundingSession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	for _, name := range []string{"from", "to"} {
		e.mustRun("run", "--session", name, "-d", "--", "/bin/sh", "-c", "sleep 120")
	}
	e.waitFor("both sessions to be running", 15*time.Second, func() bool {
		a, okA := e.session("from")
		b, okB := e.session("to")
		return okA && okB && a.State == "running" && b.State == "running"
	})

	r := e.run("switch", "to", "--from", "from")
	want := "cm: no window is using from, so nothing moved\n"
	if r.code != 0 || r.stdout != "" || r.stderr != want {
		t.Errorf("switch --from = code %d, stdout %q, stderr %q, want code 0, no stdout, stderr %q",
			r.code, r.stdout, r.stderr, want)
	}
}
