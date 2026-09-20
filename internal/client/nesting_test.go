package client

import (
	"bytes"
	"strings"
	"testing"

	"github.com/creack/pty"

	"github.com/chancez/cm/internal/osc"
)

// Both intercepted keys go to the inner client, whichever way the nesting was learned.
//
// The handover is uniform on purpose. An announced nesting is less trustworthy than one the server was told
// about, since no withdrawal is guaranteed, and that is answered by the escape below rather than by keeping
// a key back: a rule per case is one more thing to explain and to get wrong.
func TestInputGateNestingHandsOverBothKeys(t *testing.T) {
	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey(%q) error = %v", DefaultPrefixKey, err)
	}

	g := newGateWithPrefix(t, DefaultDetachKey, DefaultPrefixKey)
	g.setSuspended(true)

	if dec := g.feed([]byte{prefix.Byte}, t0); string(dec.Forward) != string([]byte{prefix.Byte}) ||
		dec.Action != gateNone {
		t.Errorf("feed(prefix) = %+v, want it forwarded: the overlay belongs to the session on screen", dec)
	}
	if dec := g.feed([]byte("\x1c"), t0); string(dec.Forward) != "\x1c" || dec.Action != gateNone {
		t.Errorf("feed(ctrl-\\) = %+v, want it forwarded: the inner client acts on it", dec)
	}
}

// A handover nobody is acting on is escapable, and the third press is what escapes it.
//
// The state this exists for: the inner client is gone and its parent does not know, so every press is
// forwarded into nothing and the window cannot be left. Two presses are silent and forwarded, because the
// inner client may be alive and merely slow; the second is reported so the user is told what the next one
// will do; the third acts here.
func TestInputGateNestedPressesEscalate(t *testing.T) {
	g := newGateWithPrefix(t, DefaultDetachKey, DefaultPrefixKey)
	g.setSuspended(true)

	if dec := g.feed([]byte("\x1c"), t0); string(dec.Forward) != "\x1c" || dec.Action != gateNone {
		t.Fatalf("first press = %+v, want it forwarded silently", dec)
	}
	if dec := g.feed([]byte("\x1c"), t0); string(dec.Forward) != "\x1c" || dec.Action != gateNestedWarn {
		t.Fatalf("second press = %+v, want it forwarded with gateNestedWarn", dec)
	}
	if dec := g.feed([]byte("\x1c"), t0); len(dec.Forward) != 0 || dec.Action != gateDetach {
		t.Fatalf("third press = %+v, want a detach and the key not forwarded", dec)
	}
}

// The count resets whenever the nesting changes, which is what keeps the escape out of reach by accident.
//
// The ordinary two-press flow is the case: one press leaves the inner session, the handover ends, and the
// next press is a plain detach of this client rather than the third of a run. Without the reset, a window
// that had hosted two nested sessions would be one keystroke from detaching itself.
func TestInputGateNestedPressCountResetsWithTheHandover(t *testing.T) {
	g := newGateWithPrefix(t, DefaultDetachKey, DefaultPrefixKey)

	g.setSuspended(true)
	if dec := g.feed([]byte("\x1c"), t0); dec.Action != gateNone {
		t.Fatalf("press while nested = %+v, want it forwarded silently", dec)
	}

	// The inner client acted on it and left.
	g.setSuspended(false)
	if dec := g.feed([]byte("\x1c"), t0); dec.Action != gateDetach {
		t.Fatalf("press after the handover ended = %+v, want an ordinary detach", dec)
	}

	// And a second nesting starts from zero rather than from one press in.
	g.setSuspended(true)
	if dec := g.feed([]byte("\x1c"), t0); dec.Action != gateNone {
		t.Errorf("first press of a new handover = %+v, want it forwarded silently", dec)
	}
}

// A burst in one read is one press, so a paste of the detach key cannot reach the escape.
//
// Auto-repeat is a separate matter and is not what this covers: its events arrive as their own reads. What
// holds it off is the keyboard's initial delay, a quarter second or more before repeating begins, plus the
// notice standing between the second press and the third.
func TestInputGateCountsOnePressPerRead(t *testing.T) {
	g := newGateWithPrefix(t, DefaultDetachKey, DefaultPrefixKey)
	g.setSuspended(true)

	if dec := g.feed([]byte("\x1c\x1c\x1c\x1c"), t0); dec.Action != gateNone {
		t.Errorf("four keys in one read = %+v, want them forwarded silently: a burst is one press", dec)
	}
}

// ptyTTY returns a TTY over a real pty, so IsTerminal is true and the announcer's conditions are the ones
// a window has rather than the ones a pipe has.
func ptyTTY(t *testing.T) *TTY {
	t.Helper()
	ptmx, slave, err := pty.Open()
	if err != nil {
		t.Fatalf("pty.Open() error = %v", err)
	}
	t.Cleanup(func() {
		ptmx.Close()
		slave.Close()
	})
	tty, err := OpenTTYCooked(slave, slave)
	if err != nil {
		t.Fatalf("OpenTTYCooked() error = %v", err)
	}
	t.Cleanup(func() { tty.Close() })
	return tty
}

// newAnnouncerScreen returns a screen that paints into a buffer, which is where an announcement lands.
func newAnnouncerScreen(t *testing.T, buf *bytes.Buffer) *screen {
	t.Helper()
	return newScreen(buf, true, nil)
}

// The announcement is written, and withdrawn, through the one writer for the terminal.
//
// Through the screen rather than straight to stdout, for the reason a window title once had to learn: a
// sequence written past it can land inside a half-written sequence of the program's, and the terminal then
// prints the parameters as text.
func TestNestingAnnouncerWritesThroughTheScreen(t *testing.T) {
	var buf bytes.Buffer
	n := &nestingAnnouncer{id: "abc123", out: newAnnouncerScreen(t, &buf)}

	n.announce()
	n.withdraw()

	want := string(osc.NestingSequence("abc123", false)) + string(osc.NestingSequence("abc123", true))
	if got := buf.String(); got != want {
		t.Errorf("written = %q, want %q", got, want)
	}
}

// A repeat is what a reconnect looks like, and it must carry the same nonce: the parent deduplicates by it,
// and a fresh one per connection would have the parent accumulate a client per server restart.
func TestNestingAnnouncerRepeatsOneNonce(t *testing.T) {
	var buf bytes.Buffer
	n := &nestingAnnouncer{id: "abc123", out: newAnnouncerScreen(t, &buf)}

	n.announce()
	n.announce()

	one := string(osc.NestingSequence("abc123", false))
	if got := buf.String(); got != one+one {
		t.Errorf("written = %q, want the same announcement twice", got)
	}
}

// A nil announcer is the ordinary case for every client that has nothing to announce, so both calls have
// to be safe on one.
func TestNilNestingAnnouncerIsSafe(t *testing.T) {
	var n *nestingAnnouncer
	n.announce()
	n.withdraw()
}

func TestNewNestingAnnouncer(t *testing.T) {
	tests := []struct {
		name string
		opts Options
		want bool
	}{
		{
			name: "an ordinary attach announces",
			opts: Options{},
			want: true,
		},
		{
			name: "a client that named its parent does not",
			// Its own server already knows, by the route that also guarantees a withdrawal. Announcing as
			// well would have the parent count one client twice.
			opts: Options{InsideSession: "@a7k2m9x4"},
			want: false,
		},
		{
			name: "a follower streaming output does not",
			// NoRestore and an Output writer are the follower: `cm read --follow` puts these bytes in a
			// file, where an OSC is corruption rather than information.
			opts: Options{NoRestore: true, Output: &bytes.Buffer{}},
			want: false,
		},
		{
			name: "a read-only client with no keys does not",
			// Nothing to be handed: it holds no detach key, so there is no handover for a parent to make.
			opts: Options{ReadOnly: true, DetachKey: KeySpec{Disabled: true}},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tty := ptyTTY(t)
			got := newNestingAnnouncer(tt.opts, tty, newAnnouncerScreen(t, &bytes.Buffer{}))
			if (got != nil) != tt.want {
				t.Errorf("newNestingAnnouncer() non-nil = %v, want %v", got != nil, tt.want)
			}
		})
	}
}

// The nonce has to be inside the character set the parser accepts, or the parent drops every announcement
// while both sides look correct.
func TestNewNestingIDIsParseable(t *testing.T) {
	for i := 0; i < 64; i++ {
		id := newNestingID()
		var tr osc.ReportTracker
		tr.Feed(osc.NestingSequence(id, false))
		got := tr.TakeNesting()
		if len(got) != 1 || got[0].ID != id {
			t.Fatalf("an announcement carrying id %q parsed as %+v", id, got)
		}
		if strings.ContainsAny(id, ";=\x1b\a") {
			t.Fatalf("id %q contains a byte that delimits the sequence", id)
		}
	}
}
