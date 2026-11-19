package e2e

import (
	"encoding/json"
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

// A session suspended with `cm kill --keep --expire-after never` reports the retention, and reviving it
// clears it.
//
// End to end because the revive is the half no unit test reaches: it replaces the record inside
// Manager.create, which needs a real shim, and a revive that carried the retention over would leave every
// session ever suspended immune to expiry for good. Both readings a suspend script uses are asserted, the
// bare --field and the JSON key.
func TestSuspendedSessionsRetentionLastsUntilRevived(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	expiry := func() (field, fromJSON string) {
		t.Helper()
		var info map[string]any
		if err := json.Unmarshal([]byte(e.mustRun("info", "suspend", "--json")), &info); err != nil {
			t.Fatalf("info --json is not JSON: %v", err)
		}
		got, ok := info["expire_after"].(string)
		if !ok {
			t.Fatalf("info --json expire_after = %#v, want a string", info["expire_after"])
		}
		return e.mustRun("info", "suspend", "--field", "expire_after"), got
	}

	e.mustRun("attach", "--no-attach", "--persist", "suspend", "--", "/bin/sh", "-c", "exec sleep 30")
	if field, fromJSON := expiry(); field != "\n" || fromJSON != "" {
		t.Errorf("before the kill, expire_after = %q / %q, want empty", field, fromJSON)
	}

	e.mustRun("kill", "suspend", "--keep", "--expire-after", "never")
	e.waitFor("the session to have ended", 15*time.Second, func() bool {
		s, ok := e.session("suspend")
		return ok && s.State == "exited"
	})
	if field, fromJSON := expiry(); field != "never\n" || fromJSON != "never" {
		t.Errorf("suspended, expire_after = %q / %q, want never", field, fromJSON)
	}

	e.mustRun("attach", "--no-attach", "suspend", "--", "/bin/sh", "-c", "exec sleep 30")
	e.waitFor("the session to be running again", 15*time.Second, func() bool {
		s, ok := e.session("suspend")
		return ok && s.State == "running"
	})
	if field, fromJSON := expiry(); field != "\n" || fromJSON != "" {
		t.Errorf("revived, expire_after = %q / %q, want empty", field, fromJSON)
	}

	e.mustRun("kill", "suspend")
}
