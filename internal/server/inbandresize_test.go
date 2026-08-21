package server

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/vt"
)

// inBandFixture returns a session whose program has enabled mode 2048, plus the fake behind it.
//
// The shim is real, because the assertion is about bytes reaching the pty and the pty echo is how a test
// can see them. The terminal is fake so the mode can be set without driving a real program's startup
// handshake.
func inBandFixture(t *testing.T, name string) (*Session, *fakeTerminal) {
	t.Helper()
	rec := startShimFor(t, shim.Config{
		Session: name,
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	// Deliberately no `answers`: a fakeTerminal that replies to any Write containing a query answers the
	// pty's own echo of its reply, forever, and the extra sequences read as a failure of whatever is under
	// test. queryorder_test.go records that trap.
	term := &fakeTerminal{restore: []byte("R"), inBandResize: true}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	t.Cleanup(func() { sess.Close() })
	return sess, term
}

// A resize must tell a program that asked to be told in band.
//
// This is the regression test for nvim freezing at one size. A program that sets mode 2048 stops acting on
// SIGWINCH and waits for CSI 48 ; rows ; cols ; ypixel ; xpixel t, so the pty ioctl alone leaves it
// drawing at the old size indefinitely. Measured before the fix: nvim held 30x100 through resizes to
// 14x99, 9x89, 15x109, and 11x79, with the pty correct at every step, and one hand-fed report moved it at
// once.
//
// Asserted on the sequence reaching the pty rather than on the fake being called, because "cm generated a
// report" and "the program received one" are different claims and only the second one fixes the bug.
func TestResizeReportsTheNewSizeInBand(t *testing.T) {
	sess, _ := inBandFixture(t, "inband")

	if err := sess.Resize(context.Background(), 30, 100, 0, 0); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// The pty echoes control bytes in caret notation, so the raw form never appears in the stream. That
	// trap has produced tests here that asserted the absence of something that could not appear either
	// way; docs/testing.md records it.
	want := "[48;30;100;0;0t"
	got := awaitStream(t, sess, want, 3*time.Second)
	if !strings.Contains(got, want) {
		t.Errorf("no in-band size report reached the pty after a resize; stream was %q, want it to contain %q.\n"+
			"A program that set mode 2048 has stopped acting on SIGWINCH and is waiting for this sequence, so "+
			"without it the program keeps drawing at its old size no matter how often the pty is resized.", got, want)
	}
}

// A program that never asked must not be sent reports.
//
// The other half of the behaviour, and the half a fix can silently break: sending an unrequested report
// puts bytes on the pty the program has no reason to read, and a shell at a prompt echoes them as text.
// That is the same shape as the recorded DA1 reply printing "62;52;c" beside a zsh prompt.
func TestResizeSendsNoReportWhenTheProgramDidNotAsk(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "noinband",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	// inBandResize left false, standing in for every program that never enables the mode, which is nearly
	// all of them.
	term := &fakeTerminal{restore: []byte("R")}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	if err := sess.Resize(context.Background(), 30, 100, 0, 0); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// A negative assertion needs a bound rather than a poll that returns early, so this waits the full
	// window and then checks. awaitStream returns as soon as it matches, so a match here is a real one.
	if got := awaitStream(t, sess, "[48;", time.Second); strings.Contains(got, "[48;") {
		t.Errorf("a size report reached the pty for a program that never enabled mode 2048; stream was %q.\n"+
			"An unrequested reply is echoed as text by a shell at a prompt, which is how a DA1 answer once "+
			"printed \"62;52;c\" beside a prompt.", got)
	}
}

// Every resize is reported, not just the first.
//
// The promise mode 2048 makes is about *every* resize, and a program that receives some reports and not
// others is worse off than one relying on SIGWINCH, which the kernel delivers every time. A fix that
// reported once and then latched would pass a single-resize test while leaving the original symptom in
// place after the second window change.
func TestEveryResizeIsReported(t *testing.T) {
	sess, _ := inBandFixture(t, "inbandmulti")

	sizes := []struct{ rows, cols uint32 }{{30, 100}, {14, 99}, {40, 120}}
	for _, s := range sizes {
		if err := sess.Resize(context.Background(), s.rows, s.cols, 0, 0); err != nil {
			t.Fatalf("Resize(%d, %d) error = %v", s.rows, s.cols, err)
		}
	}

	// Waiting for the last one, then asserting all three are present and ordered. Waiting for each in turn
	// would pass on a stream that delivered them out of order.
	last := "[48;40;120;0;0t"
	got := awaitStream(t, sess, last, 3*time.Second)
	prev := -1
	for _, s := range []string{"[48;30;100;0;0t", "[48;14;99;0;0t", last} {
		at := strings.Index(got, s)
		if at < 0 {
			t.Fatalf("report %q never reached the pty; stream was %q.\n"+
				"Mode 2048 promises a report for every resize, so a program missing one is left at a stale size.", s, got)
		}
		if at < prev {
			t.Errorf("report %q arrived out of order; stream was %q.\n"+
				"A program reads these in the order the resizes happened and takes the last as current, so "+
				"reordering leaves it at the wrong size.", s, got)
		}
		prev = at
	}
}

// A report must not overtake a question the program is still waiting on.
//
// A size report is a reply the program did not ask for, arriving at a moment it may well be mid-query, so
// it is exactly the shape that caused the `wallfacer -h` corruption: a reply consumed as the answer to a
// different question, with the real answer then arriving unclaimed and printed by the line editor.
//
// The resize path is a new writer of pty bytes, so it has to respect the same queue as every other reply.
// Writing directly would be the obvious implementation and would reintroduce that bug for any program that
// resizes while a proxied query is out, which is common: a resize makes a shell redraw, and a prompt hook
// queries the terminal.
func TestASizeReportWaitsForAnOutstandingProxiedQuery(t *testing.T) {
	sess, _ := inBandFixture(t, "inbandorder")

	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	defer sess.detach(att)

	// The program asks the background colour, which only a real terminal can answer, so it becomes an
	// outstanding request.
	sess.noteQueries([]byte("\x1b]11;?\x07"))
	select {
	case <-att.queries:
	case <-time.After(2 * time.Second):
		t.Fatal("the client was never asked the OSC 11 query, so the ordering cannot be tested")
	}

	if err := sess.Resize(context.Background(), 30, 100, 0, 0); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// Checked inside requestTimeout: waiting past it lets the sweeper legitimately release the queued
	// reply, which would read as an ordering failure rather than the expiry it is.
	report := "[48;30;100;0;0t"
	if got := awaitStream(t, sess, report, requestTimeout/2); strings.Contains(got, report) {
		t.Errorf("a size report reached the pty while an OSC 11 question was still out; stream was %q.\n"+
			"The program is blocked reading the colour, so it will consume this report as though it were "+
			"that answer. That is the recorded `wallfacer -h` corruption.", got)
	}

	// Once the client answers, both go out, colour first.
	sess.answerFromClient(att.token, []byte("\x1b]11;rgb:2828/2c2c/3434\x07"))

	got := awaitStream(t, sess, report, 3*time.Second)
	colour := strings.Index(got, "]11;rgb:2828/2c2c/3434")
	at := strings.Index(got, report)
	if colour < 0 || at < 0 {
		t.Fatalf("both the colour reply and the size report should have reached the pty; stream was %q", got)
	}
	if colour > at {
		t.Errorf("the size report was written before the colour reply; stream was %q.\n"+
			"The program asked the colour first, so it must be answered first.", got)
	}
}

// A resize must succeed even when the mode cannot be read.
//
// The resize has already reached the pty by the time the report is considered, so the size change itself is
// done. Failing the call at that point would report an error for an operation that succeeded, and would
// turn a stale repaint into a broken session for the caller.
func TestResizeSucceedsWhenTheModeCannotBeRead(t *testing.T) {
	rec := startShimFor(t, shim.Config{
		Session: "inbanderr",
		Command: []string{"/bin/sh", "-c", "sleep 10"},
		Rows:    24, Cols: 80,
	})
	term := &fakeTerminal{restore: []byte("R"), sizeReportErr: errors.New("mode unavailable")}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()

	if err := sess.Resize(context.Background(), 30, 100, 0, 0); err != nil {
		t.Errorf("Resize() error = %v, want nil: the pty was already resized, so a failure to read the "+
			"in-band mode must not fail the resize", err)
	}

	// The resize really did land, which is the part that must hold regardless. Asserted rather than assumed,
	// since an implementation that gave up before resizing would also make the error check above pass.
	term.mu.Lock()
	rows, cols := term.rows, term.cols
	term.mu.Unlock()
	if rows != 30 || cols != 100 {
		t.Errorf("model size = %dx%d, want 30x100: the resize must complete even when the mode read fails",
			rows, cols)
	}
}

// The report cm sends and the report cm drops from the model must be the same shape.
//
// Two pieces of code know this sequence: SizeReport writes it and dropSizeReports recognizes it. If they
// drift, cm either drops its own reports or stops dropping the model's untimely ones, and both failures are
// silent. Pinning them against each other is cheaper than discovering the drift through a rendering bug.
func TestTheReportCmSendsIsTheShapeCmDrops(t *testing.T) {
	report := vt.SizeReport(30, 100)
	if got := vt.DenyModes(report); len(got) != 0 {
		t.Errorf("DenyModes(SizeReport(30, 100)) = %q, want it consumed entirely.\n"+
			"SizeReport and dropSizeReports must agree on the sequence's shape, or cm drops its own "+
			"reports or delivers the model's out-of-turn ones.", got)
	}
}
