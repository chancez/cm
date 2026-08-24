package client

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// fixedSize is a terminal size for a notice under test, so nothing has to own a real terminal.
func fixedSize(rows, cols uint16) func() (uint16, uint16) {
	return func() (uint16, uint16) { return rows, cols }
}

// The whole byte sequence is asserted rather than a substring of it, because every part carries a
// requirement: DECSC and DECRC so the session's cursor is where it was, an absolute move to the last row,
// an erase so a shorter notice leaves no tail, and a reset so the session does not inherit reverse video.
func TestOutageNoticePaintsTheBottomRow(t *testing.T) {
	var buf bytes.Buffer
	n := &outageNotice{out: &buf, size: fixedSize(24, 80), enabled: true, quietFor: reconnectQuietPeriod}

	n.update(8*time.Second, "")

	want := "\x1b7\x1b[24;1H\x1b[2K\x1b[7m cm: lost the server, reconnecting (8s) \x1b[0m\x1b8"
	if got := buf.String(); got != want {
		t.Errorf("painted %q\nwant     %q", got, want)
	}
	if !n.painted {
		t.Error("painted = false after painting")
	}
}

// Nothing is painted while an outage is still routine. A server restart takes about 450ms, so a notice
// that appeared for it would flash on every upgrade, which is the case the quiet period exists for.
func TestOutageNoticeSilentDuringTheQuietPeriod(t *testing.T) {
	var buf bytes.Buffer
	n := &outageNotice{out: &buf, size: fixedSize(24, 80), enabled: true, quietFor: reconnectQuietPeriod}

	n.update(reconnectQuietPeriod-time.Millisecond, "")

	if buf.Len() != 0 {
		t.Errorf("wrote %q during the quiet period, want nothing", buf.String())
	}
	if n.painted {
		t.Error("painted = true, want false: nothing was written")
	}
}

// A follower streams bytes to a pipe, where an escape sequence is corruption rather than information.
func TestOutageNoticeSilentWhenNotPaintingATerminal(t *testing.T) {
	var buf bytes.Buffer
	n := &outageNotice{out: &buf, size: fixedSize(24, 80), enabled: false, quietFor: reconnectQuietPeriod}

	n.update(time.Minute, "")

	if buf.Len() != 0 {
		t.Errorf("wrote %q with painting disabled, want nothing", buf.String())
	}
}

// A size that could not be determined means the row number would be a guess, and a guess writes into the
// middle of somebody's session.
func TestOutageNoticeSilentWithoutASize(t *testing.T) {
	var buf bytes.Buffer
	n := &outageNotice{out: &buf, size: fixedSize(0, 0), enabled: true, quietFor: reconnectQuietPeriod}

	n.update(time.Minute, "")

	if buf.Len() != 0 {
		t.Errorf("wrote %q with an unknown size, want nothing", buf.String())
	}
}

// The same second must not be repainted. A terminal being written to is a terminal that cannot be idle,
// and the loop retries once a second for as long as the outage lasts.
func TestOutageNoticeDoesNotRepaintUnchangedText(t *testing.T) {
	var buf bytes.Buffer
	n := &outageNotice{out: &buf, size: fixedSize(24, 80), enabled: true, quietFor: reconnectQuietPeriod}

	n.update(8*time.Second, "")
	first := buf.Len()
	n.update(8*time.Second+10*time.Millisecond, "")
	if buf.Len() != first {
		t.Errorf("wrote %d more bytes for an unchanged notice, want none", buf.Len()-first)
	}

	// A new second is a change, and does repaint.
	n.update(9*time.Second, "")
	if !strings.Contains(buf.String(), "(9s)") {
		t.Errorf("output = %q, want it to show the new elapsed time", buf.String())
	}
}

// clear erases the row and says it had painted, which is what tells the caller a repaint is owed.
func TestOutageNoticeClearReportsWhetherItHadPainted(t *testing.T) {
	var buf bytes.Buffer
	n := &outageNotice{out: &buf, size: fixedSize(24, 80), enabled: true, quietFor: reconnectQuietPeriod}

	if n.clear() {
		t.Error("clear() = true before anything was painted, want false")
	}
	if buf.Len() != 0 {
		t.Errorf("clear() wrote %q with nothing painted, want nothing", buf.String())
	}

	n.update(8*time.Second, "")
	buf.Reset()
	if !n.clear() {
		t.Error("clear() = false after painting, want true")
	}
	want := "\x1b7\x1b[24;1H\x1b[2K\x1b8"
	if got := buf.String(); got != want {
		t.Errorf("clear() wrote %q, want %q", got, want)
	}
}

// The line must fit inside the terminal, one column short of the width.
//
// Writing the final column leaves the terminal in its pending-wrap state, where one more byte scrolls the
// screen: that shifts the session's content up a row and desynchronizes it from the model about to repaint
// it. A newline in a reason has the same effect, so those are collapsed too.
func TestNoticeTextFitsTheWidth(t *testing.T) {
	tests := []struct {
		name   string
		cols   int
		reason string
		want   string
	}{
		{
			name: "fits",
			cols: 80,
			want: " cm: lost the server, reconnecting (5s) ",
		},
		{
			name: "truncated to one short of the width",
			cols: 20,
			want: " cm: lost the serve",
		},
		{
			name:   "a reason replaces the wait",
			cols:   80,
			reason: "unknown setting foo",
			want:   " cm: the server is not starting: unknown setting foo ",
		},
		{
			name:   "newlines in a reason are collapsed",
			cols:   80,
			reason: "line one\nline two",
			want:   " cm: the server is not starting: line one line two ",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := noticeText(5*time.Second, tt.reason, tt.cols)
			if got != tt.want {
				t.Errorf("noticeText() = %q, want %q", got, tt.want)
			}
			if len(got) >= tt.cols {
				t.Errorf("noticeText() is %d bytes for a %d column terminal, want it shorter",
					len(got), tt.cols)
			}
		})
	}
}

// A client that painted a notice must repaint the session rather than resume it.
//
// The notice overwrites the session's bottom row, and cm's terminal model is the only thing that knows
// what was there. Resuming continues the stream and never repaints, so the row would stay as this left it:
// a blank line where a prompt or a status line belongs. Dropping the position turns the reconnect into a
// fresh attach, which the server answers with a serialized screen.
//
// Driven with a notice that is already on screen rather than by waiting out the quiet period, so the test
// asserts the wiring without spending three seconds proving a threshold that is tested directly above.
func TestAttachRepaintsWhenANoticeWasOnScreen(t *testing.T) {
	svc := &stubService{handle: func(n int, srv serverv1.Server_AttachServer) error {
		if n == 1 {
			if err := sendOpened(srv, "test", 100); err != nil {
				return err
			}
			// Output, so the client holds a position worth resuming from, and then the stream ends the way
			// a server going away ends it.
			return srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Output{Output: &serverv1.Output{
					Seq: 100, Data: []byte("before"),
				}},
			})
		}
		if err := sendOpened(srv, "test", 106); err != nil {
			return err
		}
		return srv.Send(&serverv1.AttachResponse{
			Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: 0}},
		})
	}}
	socket := serveStub(t, svc)
	tty, opts := attachOpts(t, socket)

	var painted bytes.Buffer
	opts.notice = &outageNotice{
		out:  &painted,
		size: fixedSize(24, 80),
		// Paints on the first outage rather than after three seconds of it, so this test is about the
		// recovery and not about the threshold, which is asserted directly above.
		enabled:  true,
		quietFor: 0,
	}

	if _, err := Attach(context.Background(), tty, opts); err != nil {
		t.Fatalf("Attach() error = %v", err)
	}

	opens := svc.openMessages()
	if len(opens) != 2 {
		t.Fatalf("got %d connections, want 2: the client has to reconnect for this to be about recovery",
			len(opens))
	}
	if opens[1].ResumeFromSeq != nil {
		t.Errorf("second Open resumed from %d, want no position at all.\n"+
			"Resuming leaves the row this notice overwrote as a blank line, since a resume streams new "+
			"bytes and never repaints. Only a fresh attach rebuilds it, from the model the server holds.",
			*opens[1].ResumeFromSeq)
	}
	// The whole sequence: painted when the connection dropped, then erased when the server came back. The
	// elapsed time reads as 0s because this notice has no quiet period to wait out.
	want := "\x1b7\x1b[24;1H\x1b[2K\x1b[7m cm: lost the server, reconnecting (0s) \x1b[0m\x1b8" +
		"\x1b7\x1b[24;1H\x1b[2K\x1b8"
	if got := painted.String(); got != want {
		t.Errorf("wrote %q\nwant     %q", got, want)
	}
}
