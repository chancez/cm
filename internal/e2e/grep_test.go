package e2e

import (
	"strings"
	"testing"
	"time"
)

// `cm grep --all` finds a line in each live session and names the session it came from, through the real
// HistoryPage walk; a pattern nothing holds exits 1, as grep does.
func TestGrepSearchesEverySession(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "a", "-d", "--", "/bin/sh", "-c", "printf 'alpha\\nNEEDLE-one\\n'; sleep 120")
	e.mustRun("run", "--session", "b", "-d", "--", "/bin/sh", "-c", "printf 'NEEDLE-two\\n'; sleep 120")
	e.waitFor("both sessions to print", 15*time.Second, func() bool {
		return strings.Contains(e.run("read", "a").stdout, "NEEDLE-one") &&
			strings.Contains(e.run("read", "b").stdout, "NEEDLE-two")
	})

	r := e.run("grep", "--all", "--grep-command", "grep -n", "NEEDLE")
	if r.code != 0 {
		t.Fatalf("cm grep exited %d: %s", r.code, r.stderr)
	}
	got := strings.Split(strings.TrimSpace(r.stdout), "\n")
	want := []string{"default/a:2:NEEDLE-one", "default/b:1:NEEDLE-two"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("cm grep output:\n%s\nwant:\n%s", r.stdout, strings.Join(want, "\n"))
	}

	if r := e.run("grep", "--all", "--grep-command", "grep -n", "absent-pattern"); r.code != 1 {
		t.Errorf("cm grep with no match exited %d, want 1: %s", r.code, r.stderr)
	}
}
