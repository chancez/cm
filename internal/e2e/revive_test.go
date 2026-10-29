package e2e

import (
	"strings"
	"testing"
	"time"
)

// A revived persisted session shows each run's output once.
//
// It showed the new run's first line twice, in `cm read` and in `cm history`, three revivals out of three in
// a sandbox. The revived shim appends to the same log the server then seeds the model from, and a program
// prints faster than the seed runs, so that output came from disk and again from the pump. The fix bounds
// the seed at the log's end as read before the shim is spawned, and that ordering lives in Manager.create,
// which only a real revive goes through: the unit test beside the seed is handed its bound.
//
// DONE is printed after a pause so the assertion cannot race the duplicate. The pump delivers in order, so
// once DONE is in the history the pump's copy of SECOND_RUN already is too.
func TestRevivedSessionShowsEachRunOnce(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	e.mustRun("attach", "--no-attach", "--persist", "revive", "--",
		"/bin/sh", "-c", "echo FIRST_RUN; exec sleep 30")
	e.waitForHistory("revive", "FIRST_RUN")

	e.mustRun("kill", "revive", "--keep")
	e.waitFor("the session to have ended", 15*time.Second, func() bool {
		s, ok := e.session("revive")
		return ok && s.State == "exited"
	})

	e.mustRun("attach", "--no-attach", "revive", "--",
		"/bin/sh", "-c", "echo SECOND_RUN; sleep 0.5; echo DONE; exec sleep 30")
	history := e.waitForHistory("revive", "DONE")

	var got []string
	for _, line := range strings.Split(history, "\n") {
		if line = strings.TrimSpace(line); line != "" {
			got = append(got, line)
		}
	}
	want := []string{"FIRST_RUN", "SECOND_RUN", "DONE"}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("history after a revive = %q, want %q", got, want)
	}

	e.mustRun("kill", "revive")
}
