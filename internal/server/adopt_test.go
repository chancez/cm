package server

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/store"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// A session adopted after a server restart must have its screen rebuilt from the shim's output.
//
// The terminal model lives in the server, so a new server starts with a blank one, and consuming from
// the recorded resume point only ever sees output produced from then on. Everything the shell printed
// before the restart was therefore missing: `cm history` on an adopted session returned nothing, and a
// client reattaching got an empty screen even though the shell was fine.
//
// The bytes were never actually lost. The shim retains 4 MiB of output and reports its oldest
// retained sequence, so the screen is recoverable, which is why this is a bug rather than a limit.
func TestAdoptRebuildsScreenFromShimHistory(t *testing.T) {
	term := &fakeTerminal{}
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})
	ctx := context.Background()

	// Output produced before this server existed, which is what a restart leaves behind.
	rec := startShimFor(t, shimConfigFor("adopted", "echo BEFORE_RESTART; sleep 5"))
	rec.State = "running"
	recordSession(t, st, rec)

	// Wait for the shell's output to reach the shim, or there would be no history to replay and the
	// test would pass without exercising anything.
	waitForShimOutput(t, rec.ShimSocket, "BEFORE_RESTART")

	// Record the previous server as having consumed everything, which is what a restart leaves in the
	// store. This is what makes the test meaningful: the pump starts at the end, so replaying the
	// shim's history is the only way the model can hold anything. Leaving LastSeq at 0 would have the
	// pump deliver the output itself, and the test would pass without the code under test running.
	next := shimNextSeq(t, rec.ShimSocket)
	rec.LastSeq = next
	if err := st.Apply(ctx, rec.ID, store.Update{LastSeq: &next}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if _, ok := mgr.Get("adopted"); !ok {
		t.Fatal("session was not adopted")
	}

	// The model must hold what the shell printed before this server started.
	if got := term.Written(); !strings.Contains(got, "BEFORE_RESTART") {
		t.Errorf("terminal model = %q, want it to contain the pre-restart output", got)
	}
}

// Replayed history must not be delivered twice.
//
// The replay stops at the point the session's own pump takes over. Overlapping by even one byte would
// write the same output to the model twice, and a terminal fed duplicate output shows duplicated
// lines, which looks like a rendering bug rather than an off-by-one.
func TestAdoptDoesNotDuplicateAtTheResumeBoundary(t *testing.T) {
	term := &fakeTerminal{}
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("nodup", "echo UNIQUE_LINE; sleep 5"))
	rec.State = "running"
	recordSession(t, st, rec)
	waitForShimOutput(t, rec.ShimSocket, "UNIQUE_LINE")

	// Resume from partway through the output rather than from 0 or from the end. This is what makes
	// the boundary real: the replay must stop exactly here and the pump must continue from exactly
	// here, so an off-by-one in either direction duplicates or drops bytes. With LastSeq at 0 there is
	// no boundary to get wrong and a replay-everything implementation passes.
	next := shimNextSeq(t, rec.ShimSocket)
	half := next / 2
	if half == 0 {
		t.Fatalf("shim reported only %d bytes, too few to split at a boundary", next)
	}
	if err := st.Apply(ctx, rec.ID, store.Update{LastSeq: &half}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	// Wait for the model to settle, then assert on the exact contents.
	//
	// Deliberately not fatal on timeout. A broken implementation produces a *corrupt* screen rather
	// than a missing one, so aborting here would report "did not contain the line" and never reach
	// the check that actually distinguishes right from wrong.
	settleTerminal(term, "UNIQUE_LINE\r\n")

	// Assert on the exact contents, not a substring count.
	//
	// Counting occurrences of the line is not enough, and passed against a deliberately broken
	// implementation. Getting the boundary wrong duplicates a *fragment* rather than the whole line:
	// replaying everything and then letting the pump resume mid-line produced
	// "UNIQUE_LINE\r\n_LINE\r\n", which contains the line exactly once and is still corrupt.
	if got, want := term.Written(), "UNIQUE_LINE\r\n"; got != want {
		t.Errorf("terminal model = %q, want %q", got, want)
	}
}

// settleTerminal waits until a fake terminal's contents stop changing, or until want is exceeded.
//
// Returns rather than failing: the caller asserts on the exact contents, and a corrupt screen is what
// a regression looks like, so failing here would mask the real comparison.
func settleTerminal(term *fakeTerminal, want string) {
	deadline := time.Now().Add(2 * time.Second)
	last := ""
	stable := 0
	for time.Now().Before(deadline) {
		got := term.Written()
		if got == last && got != "" {
			stable++
			// Settled, and either correct or corrupt. Either way there is nothing more to wait for.
			if stable >= 3 && len(got) >= len(want) {
				return
			}
		} else {
			stable = 0
		}
		last = got
		time.Sleep(20 * time.Millisecond)
	}
}

// Adoption must partly resume from where the previous server stopped, not replay everything to
// clients.
//
// Two separate destinations are involved and only one gets the history. The terminal model is rebuilt
// from the shim's oldest retained byte, since it starts empty. The session's client log is not, since
// a client resuming has already seen those bytes and appending them would replay old output as though
// it were new.
func TestAdoptDoesNotReplayHistoryToClients(t *testing.T) {
	term := &fakeTerminal{}
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("noreplay", "echo OLD_OUTPUT; sleep 5"))
	rec.State = "running"
	recordSession(t, st, rec)
	waitForShimOutput(t, rec.ShimSocket, "OLD_OUTPUT")

	// A previous server that had already consumed this output records how far it got. Read the shim's
	// own count rather than guessing a byte offset.
	next := shimNextSeq(t, rec.ShimSocket)
	rec.LastSeq = next
	if err := st.Apply(ctx, rec.ID, store.Update{LastSeq: &next}); err != nil {
		t.Fatalf("Apply() error = %v", err)
	}

	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("noreplay")
	if !ok {
		t.Fatal("session was not adopted")
	}

	// The model has the history, so a reattaching client still gets a correct screen.
	if got := term.Written(); !strings.Contains(got, "OLD_OUTPUT") {
		t.Errorf("terminal model = %q, want the pre-restart output", got)
	}

	// The client log does not, so a resuming client is not shown it again.
	r := sess.recent.Subscribe(sess.recent.Oldest())
	defer r.Close()
	readCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	var seen strings.Builder
	for {
		c, err := r.Next(readCtx)
		if err != nil {
			break
		}
		seen.Write(c.Data)
	}
	if strings.Contains(seen.String(), "OLD_OUTPUT") {
		t.Errorf("client log = %q, want the already-seen output withheld", seen.String())
	}
}

// waitForShimOutput blocks until a shim's log contains want.
//
// Necessary rather than a sleep: these tests replay a shim's history, so there has to actually be
// history, and a test that raced the shell would pass without exercising the replay at all.
func waitForShimOutput(t *testing.T, socket, want string) {
	t.Helper()

	conn, shim, err := dialShim(socket)
	if err != nil {
		t.Fatalf("dialShim() error = %v", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	sub, err := shim.Subscribe(ctx, &shimv1.SubscribeRequest{FromSeq: 0})
	if err != nil {
		t.Fatalf("Subscribe() error = %v", err)
	}
	var got strings.Builder
	for {
		out, err := sub.Recv()
		if err != nil {
			t.Fatalf("waiting for %q in the shim's output, got %q: %v", want, got.String(), err)
		}
		got.Write(out.Data)
		if strings.Contains(got.String(), want) {
			return
		}
	}
}

// shimNextSeq reports the sequence number a shim will assign next, which is where a server that had
// consumed everything would resume from.
func shimNextSeq(t *testing.T, socket string) seq.Shim {
	t.Helper()

	conn, shim, err := dialShim(socket)
	if err != nil {
		t.Fatalf("dialShim() error = %v", err)
	}
	defer conn.Close()

	st, err := shim.State(context.Background(), &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}
	return seq.Shim(st.NextSeq)
}
