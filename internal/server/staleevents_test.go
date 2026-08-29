package server

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A key release and a focus report generated for a program that has exited must not reach the shell.
//
// Reported as "execute: 3u[O_" left in zsh's line editor shortly after quitting codex. codex pushes kitty
// keyboard flags 7, which includes report-event-types, and sets mode 1004. On ctrl-d it reads the key
// *press* and exits; the *release* is generated afterwards and arrives with nothing but the shell left to
// read it. zsh's line editor ate the ESC as a meta prefix and inserted the rest, so "\x1b[100;5:3u"
// showed as "3u" and "\x1b[O" as "[O".
//
// Racy in the real world, which is why this is a seam test rather than an end-to-end one: the program's
// own flag pop has to reach the terminal before the key is lifted, and under cm that is a round trip
// through the shim, the server and the client. It reproduced twice in three tries when codex was quit as
// soon as it opened. Here the state is constructed instead: the shell pushed nothing, so the model's
// flags are zero, which is exactly the state the session is in once the program has gone.
//
// Driven through Service.Attach rather than by calling the filter, because the bug was not that cm could
// not recognize these bytes: IsUserInput already calls a release not-typing. It was that nothing acted on
// that, so the chunk fell through to the verbatim pty write. Only the service exercises that decision.
func TestStaleKeyReleaseAndFocusReportDoNotReachTheShell(t *testing.T) {
	// A model reporting exactly the state a session is in once the program has gone: it pushed no
	// kitty flags and set no focus mode. Passed as a factory because newTestManager with nil gives the
	// session no model at all, and terminalModes then reports both modes *on* so nothing is filtered.
	// That is not a hypothetical: this test first passed for that reason.
	term := &fakeTerminal{}
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})
	ctx := context.Background()

	// `cat` echoes whatever reaches it, so anything forwarded comes back as output.
	rec := startShimFor(t, shimConfigFor("stale-ev", "echo READY; cat"))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("stale-ev")
	if !ok {
		t.Fatal("session was not adopted")
	}

	watch := sess.recent.Subscribe(0)
	defer watch.Close()
	readUntil(t, watch, "READY")

	// The precondition, asserted rather than assumed. If the model reported the protocol as on, the
	// filter would forward everything and this test would pass for the wrong reason.
	if modes := sess.terminalModes(); modes.KittyKeyboard || modes.FocusReports {
		t.Fatalf("sess.terminalModes() = %+v, want both off: no program in this session set either, so "+
			"the state under test was never reached", modes)
	}

	svc := NewService(mgr)
	stream := newFakeStream(ctx,
		openReq(&serverv1.Open{Session: "stale-ev", Rows: 24, Cols: 80}),
		// The reported chunk, followed by something a person really typed. Together in one message on
		// purpose: dropping the whole chunk would swallow the keystroke, which is the other way to get
		// this wrong.
		inputReq("\x1b[100;5:3u\x1b[OTYPED_BY_HAND\n"),
	)
	if err := svc.Attach(ctx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	seen := drainFor(t, watch, 700*time.Millisecond)

	// The needles are the parameters rather than the whole sequence, because a pty echoes control
	// characters in caret notation: the ESC comes back as "^[", so a needle containing a real ESC would
	// never match and the test would pass whatever cm did.
	if strings.Contains(seen, "100;5:3u") {
		t.Errorf("a stale key release reached the shell: %q.\n"+
			"No program in this session has the kitty keyboard protocol enabled, so this event was "+
			"generated for one that has exited and is typed at whatever is reading the pty now. That is "+
			"the reported \"3u\" beside the prompt.", seen)
	}
	if strings.Contains(seen, "^[[O") || strings.Contains(seen, "\x1b[O") {
		t.Errorf("a stale focus report reached the shell: %q.\n"+
			"Mode 1004 is off, so nothing asked to be told about focus. That is the reported \"[O\".", seen)
	}
	if !strings.Contains(seen, "TYPED_BY_HAND") {
		t.Errorf("real typing in the same chunk as a stale event was lost: %q.\n"+
			"The filter must drop the stale sequences and keep everything else, since a terminal writes "+
			"a burst of events and keystrokes together.", seen)
	}
}

// The control: while a program does have the protocol enabled, a release is what it asked for.
//
// Without this the filter could drop every release unconditionally and the test above would still pass,
// which would break exactly the programs that use flags 7 on purpose. The two tests differ only in
// whether the shell pushed flags.
func TestKeyReleaseReachesAProgramThatAskedForIt(t *testing.T) {
	term := &fakeTerminal{kittyKeyboard: true}
	mgr, st, _ := newTestManager(t, func(rows, cols uint16) (Terminal, error) {
		return term, nil
	})
	ctx := context.Background()

	rec := startShimFor(t, shimConfigFor("live-ev", "echo READY; cat"))
	rec.State = "running"
	recordSession(t, st, rec)
	if err := mgr.Reconcile(ctx); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	sess, ok := mgr.Get("live-ev")
	if !ok {
		t.Fatal("session was not adopted")
	}

	watch := sess.recent.Subscribe(0)
	defer watch.Close()
	readUntil(t, watch, "READY")

	// The precondition for a control is the one most worth asserting: if this reported the protocol
	// off, the release would be filtered and the test would "pass" by testing the wrong branch.
	if modes := sess.terminalModes(); !modes.KittyKeyboard {
		t.Fatalf("sess.terminalModes() = %+v, want KittyKeyboard on, so this control exercises the "+
			"forwarding branch rather than the filtering one", modes)
	}

	svc := NewService(mgr)
	stream := newFakeStream(ctx,
		openReq(&serverv1.Open{Session: "live-ev", Rows: 24, Cols: 80}),
		inputReq("\x1b[100;5:3u"),
	)
	if err := svc.Attach(ctx, stream); err != nil && !errors.Is(err, io.EOF) {
		t.Fatalf("Attach() error = %v", err)
	}

	seen := drainFor(t, watch, 2*time.Second)
	if !strings.Contains(seen, "100;5:3u") {
		t.Errorf("a key release was dropped while a program had the protocol enabled: %q.\n"+
			"Flags 7 includes report-event-types, so this program asked to see releases and dropping one "+
			"breaks it.", seen)
	}
}

// drainFor collects everything the session produces for a fixed window.
//
// A window rather than a wait for a needle, because these tests assert an *absence*: there is nothing to
// wait for, and returning early would mean the bytes simply had not arrived yet.
func drainFor(t *testing.T, watch *seqlog.Reader[seq.Log], d time.Duration) string {
	t.Helper()
	deadline, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	var seen strings.Builder
	for {
		c, err := watch.Next(deadline)
		if err != nil {
			return seen.String()
		}
		seen.WriteString(string(c.Data))
	}
}
