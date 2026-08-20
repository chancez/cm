package server

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// A session's environment is the server's with the client's appended, which is an override only
// because exec keeps the last occurrence of a duplicated name. That is a property of the standard
// library rather than of cm, and the whole fix for a session inheriting a stale environment rests on
// it, so it is asserted rather than assumed: if it ever changed, sessions would silently go back to
// keeping the server's values and every other test here would still pass.
//
// Run against a real process because the behavior being checked is the exec syscall's, which no
// amount of inspecting cmd.Env can show.
//
// Built the same way spawnShim builds it, deliberately close to the real call site: this used to test
// a shimEnv helper, and the helper turned out to be unnecessary once a client forwarded its whole
// environment, leaving the one-line append the server always had.
func TestExecKeepsTheLastDuplicateEnvEntry(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skipf("no /bin/sh: %v", err)
	}

	cmd := exec.Command("/bin/sh", "-c", "printf %s \"$CM_TEST_PROBE\"")
	cmd.Env = append([]string{"CM_TEST_PROBE=server-stale"}, "CM_TEST_PROBE=client-fresh")

	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("Output() error = %v", err)
	}
	if got := strings.TrimSpace(string(out)); got != "client-fresh" {
		t.Errorf("child saw CM_TEST_PROBE=%q, want %q; appending no longer overrides, so a "+
			"session would keep the server's stale value", got, "client-fresh")
	}
}
