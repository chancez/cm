package server

import (
	"context"
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/store"
)

// restartAdopting is the shape of a server restart: run a first session against a shim, let it consume
// output, then adopt the same shim from where the first left off.
//
// Returned as the adopted session, since that is what every test here asserts on. The first session is
// closed before the second exists, matching a restart: two servers never hold one shim at once.
func restartAdopting(t *testing.T, name, script, marker string) *Session {
	t.Helper()

	mgr, st, _ := newTestManager(t, nil)
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor(name, script))
	rec.State = store.StateRunning
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	first, ok := mgr.Get(rec.ID)
	if !ok {
		t.Fatal("session was not adopted by the first server")
	}

	// Waiting for output the script prints after its markers, so the markers are known to have reached
	// the first session rather than merely been written. Without this the shim's retained log might not
	// hold them yet and the test would prove nothing.
	sub := first.recent.Subscribe(0)
	readUntil(t, sub, marker)
	sub.Close()

	rec.LastSeq, rec.ClientSeq = first.resumePoints()
	first.Close()

	second, err := mgr.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq, "")
	if err != nil {
		t.Fatalf("adopt() error = %v", err)
	}
	t.Cleanup(second.Close)
	return second
}

// A session running a command still says what it is running after a server restart.
//
// This is the regression test. `cm list` derives its STATE column from OSC 133 markers as they stream
// past, and that state lives in the server, so a restart started every session over with a blank one:
// a shell that had been running `make` for an hour showed plain "running". The markers are still in the
// shim's retained log, which adoption already reads to rebuild the screen, so the state was recoverable
// all along and simply was not being read.
func TestAdoptionRecoversTheRunningCommand(t *testing.T) {
	sess := restartAdopting(t, "cmdstate",
		// A prompt, then a command that has started and not finished. MARK is ordinary output after the
		// markers, which is what the helper waits for.
		"printf '\\033]133;A\\007$ '; printf '\\033]133;C;cmdline=make\\ world\\007'; "+
			"printf 'MARK\\r\\n'; sleep 5",
		"MARK")

	// The whole state, not just Running: Command is what the column prints, and Runs is what a `send
	// --wait` measures against, so a recovery that dropped either would still read as working here.
	want := osc.CommandState{Running: true, Command: "make world", Runs: 1}
	if got := sess.Command(); got != want {
		t.Errorf("Command() = %+v, want %+v.\n"+
			"An adopted session's command state comes from replaying the shim's retained output through "+
			"the OSC tracker. A zero value means the replay fed the terminal model and nothing else, "+
			"which is what made every restart blank the STATE column.", got, want)
	}
}

// A command that finished before the restart is reported as finished, with its status.
//
// The complement of the test above, and not the same code path in what it proves: recovering "running"
// only requires seeing the C marker, while this requires the D marker after it to have been applied in
// order. A replay that fed chunks out of order, or stopped at the first marker, passes the other test.
func TestAdoptionRecoversTheLastExitStatus(t *testing.T) {
	sess := restartAdopting(t, "exitstate",
		"printf '\\033]133;A\\007$ '; printf '\\033]133;C;cmdline=false\\007'; "+
			"printf '\\033]133;D;1\\007'; printf 'MARK\\r\\n'; sleep 5",
		"MARK")

	want := osc.CommandState{Running: false, Command: "", ExitCode: 1, Exited: true, Runs: 1}
	if got := sess.Command(); got != want {
		t.Errorf("Command() = %+v, want %+v.\n"+
			"The last command's status is what `cm list` shows as exited(1) and what a script reads from "+
			"last_command_exit_code. It has to survive a restart for the same reason the command does.",
			got, want)
	}
}

// A report a shell integration wrote into the stream survives a restart too.
//
// Reports arrive two ways and end in one field: cm's own OSC 25453, which is in the output stream, and
// `cm report`, which is an RPC. The first is recoverable from the log exactly like OSC 133, and this is
// the case that matters for a shell hook, since the hook only writes the sequence again on the next
// state change.
func TestAdoptionRecoversAReportedState(t *testing.T) {
	// MARK2 comes after a pause so it is certainly live output for the adopted session rather than part
	// of the history the replay covers: the first session's resume position is taken the moment MARK
	// arrives, and anything printed later is past it however long the adoption takes.
	sess := restartAdopting(t, "reportstate",
		"printf '\\033]25453;state=blocked;detail=needs\\ approval;source=agent\\007'; "+
			"printf 'MARK\\r\\n'; sleep 0.5; printf 'MARK2\\r\\n'; sleep 5",
		"MARK")

	want := Reported{State: "blocked", Detail: "needs approval", Source: "agent"}
	if got := sess.Reported(); got != want {
		t.Errorf("Reported() = %+v, want %+v.\n"+
			"A blocked program is still blocked after a server restart: nothing about the restart tells it "+
			"to report again, so forgetting the report leaves the session looking idle while it waits.",
			got, want)
	}

	// Recovered, and not re-delivered as news. The tracker holds one report and Take drains it, so a
	// recovery that left it in place would have the first chunk of live output publish it again as a
	// change, which a `cm wait` reads as the program having just moved.
	sub := sess.recent.Subscribe(sess.recent.Next())
	readUntil(t, sub, "MARK2")
	sub.Close()
	sess.mu.Lock()
	runs := sess.reportRuns
	sess.mu.Unlock()
	if runs != 0 {
		t.Errorf("reportRuns = %d after adoption and live output, want 0.\n"+
			"A recovered report is state this session inherited, not an event it observed. Counting it "+
			"satisfies a wait that was issued afterwards, which is the already-in-state bug.", runs)
	}
}
