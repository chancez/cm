package client

import (
	"bytes"
	"strings"
	"testing"

	"github.com/creack/pty"

	"github.com/chancez/cm/internal/osc"
)

// A client that announced itself can only be withdrawn by itself, so a window whose announcement is never
// withdrawn must keep one way to reach cm. The detach key still goes to the inner client; the overlay's
// prefix stays here.
//
// The failure this prevents: a dropped ssh leaves the parent believing a client is nested, and with both
// keys handed over that window answers neither ctrl-\ nor ctrl-], so it can only be freed from another
// window.
func TestInputGateAnnouncedNestingKeepsThePrefixKey(t *testing.T) {
	g := newGateWithPrefix(t, DefaultDetachKey, DefaultPrefixKey)
	g.suspended, g.keepPrefix = true, true

	// The detach key is an ordinary keystroke now, and reaches the inner client whole.
	if dec := g.feed([]byte("\x1c"), t0); string(dec.Forward) != "\x1c" || dec.Action != gateNone {
		t.Errorf("feed(ctrl-\\) = %+v, want it forwarded with no action: the inner client acts on it", dec)
	}

	// The prefix key is still this client's.
	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey(%q) error = %v", DefaultPrefixKey, err)
	}
	if dec := g.feed([]byte{prefix.Byte}, t0); dec.Action != gatePrefix {
		t.Errorf("feed(prefix) = %+v, want gatePrefix: an announced nesting leaves the overlay reachable", dec)
	}
}

// An attachment the server was told about hands over both keys, which is the behaviour that predates
// announcements and must not change: the overlay belongs to the session being looked at.
func TestInputGateKnownNestingHandsOverBothKeys(t *testing.T) {
	g := newGateWithPrefix(t, DefaultDetachKey, DefaultPrefixKey)
	g.suspended, g.keepPrefix = true, false

	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey(%q) error = %v", DefaultPrefixKey, err)
	}
	if dec := g.feed([]byte{prefix.Byte}, t0); string(dec.Forward) != string([]byte{prefix.Byte}) ||
		dec.Action != gateNone {
		t.Errorf("feed(prefix) = %+v, want it forwarded: the inner client owns the overlay too", dec)
	}
}

// While only the prefix is this client's, a partial that could be either key must not be withheld for the
// detach key's sake: this client will not act on it, and the inner client's own gate is waiting for it.
func TestInputGateAnnouncedNestingHoldsOnlyForThePrefix(t *testing.T) {
	g := newGateWithPrefix(t, "ctrl-\\", "ctrl-]")
	g.suspended, g.keepPrefix = true, true

	// A lone escape is a prefix of the CSI encodings of both keys. Held, because the prefix key could still
	// arrive, and released in order afterwards.
	if dec := g.feed([]byte("\x1b"), t0); len(dec.Forward) != 0 {
		t.Errorf("feed(escape) = %+v, want it withheld while the prefix key is still live", dec)
	}
	if _, holding := g.deadline(); !holding {
		t.Error("deadline() reports nothing held, so a CSI-encoded prefix key would be missed")
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
