package e2e

import (
	"strings"
	"testing"
	"time"
)

// `cm forget <TAB>` offers the sessions it will accept, and only those.
//
// Through cobra's own `__complete`, which is what a shell runs, rather than by calling the completion
// function: the two properties being checked are both decided by wiring that a direct call bypasses, which
// command's ValidArgsFunction is installed and what its usage line says.
//
// Running sessions are excluded because forget refuses them. A completion that offers what the command
// rejects reads as the command being broken rather than the choice being wrong.
func TestForgetCompletesOnlyEndedSessions(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("run", "--session", "over", "-d", "--", "/bin/sh", "-c", "exit 0")
	e.mustRun("run", "--session", "ongoing", "-d", "--", "/bin/sh", "-c", "sleep 120")
	e.waitFor("the finished session to be recorded as exited", 15*time.Second, func() bool {
		s, ok := e.session("over")
		return ok && s.State == "exited"
	})

	offers := e.mustRun("__complete", "forget", "")
	if !strings.Contains(offers, "over") {
		t.Errorf("offers = %q, want the ended session offered", offers)
	}
	if strings.Contains(offers, "ongoing") {
		t.Errorf("offers = %q, want the running session left out: forget would refuse it", offers)
	}

	// And a second name still completes, since forget takes several. This is what a name check on "kill"
	// got wrong: forget was not that name, so `cm forget a b<TAB>` offered nothing at all.
	e.mustRun("run", "--session", "over2", "-d", "--", "/bin/sh", "-c", "exit 0")
	e.waitFor("the second finished session to be recorded as exited", 15*time.Second, func() bool {
		s, ok := e.session("over2")
		return ok && s.State == "exited"
	})

	second := e.mustRun("__complete", "forget", "over", "")
	if !strings.Contains(second, "over2") {
		t.Errorf("offers after one name = %q, want the other ended session still offered", second)
	}
	// And not the one already typed, which would complete to a duplicate.
	for _, line := range strings.Split(second, "\n") {
		if name, _, _ := strings.Cut(line, "\t"); name == "over" {
			t.Errorf("offers after one name = %q, want the name already given dropped", second)
		}
	}
}
