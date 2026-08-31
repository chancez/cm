package client

import (
	"testing"
	"time"
)

// t0 is an arbitrary fixed instant, so a deadline can be asserted exactly rather than approximately.
var t0 = time.Date(2026, 8, 22, 12, 0, 0, 0, time.UTC)

func newGate(t *testing.T, spec string) *inputGate {
	t.Helper()
	key, err := ParseDetachKey(spec)
	if err != nil {
		t.Fatalf("ParseDetachKey(%q) error = %v", spec, err)
	}
	return &inputGate{key: key}
}

func TestInputGateFeed(t *testing.T) {
	// The default key, so the encodings are the real ones: 0x1C, and the CSI forms a terminal sends
	// when a program has enabled the kitty protocol or modifyOtherKeys.
	type step struct {
		in string
		// want is what the session should receive from this read.
		want string
		// wantDetach reports that the key was pressed.
		wantDetach bool
		// wantHeld is what stays withheld afterwards.
		wantHeld string
	}

	tests := []struct {
		name  string
		steps []step
	}{
		{
			// The bug. Escape is a one-byte prefix of both CSI encodings, so it is withheld, and
			// before the grace existed nothing ever released it.
			name: "a lone escape is withheld, not forwarded",
			steps: []step{
				{in: "\x1b", want: "", wantHeld: "\x1b"},
			},
		},
		{
			name: "the control byte detaches immediately",
			steps: []step{
				{in: "\x1c", want: "", wantDetach: true},
			},
		},
		{
			// A keystroke typed just before the key still reaches the session, since the user meant
			// to type it.
			name: "typing before the key is forwarded",
			steps: []step{
				{in: "ls\x1c", want: "ls", wantDetach: true},
			},
		},
		{
			// Bytes after the key are dropped on purpose: the user asked to leave.
			name: "typing after the key is dropped",
			steps: []step{
				{in: "\x1cls", want: "", wantDetach: true},
			},
		},
		{
			// What the holdback is for. Split across two reads, the sequence is still recognized
			// rather than half-forwarded to the shell.
			name: "a CSI encoding split across reads still detaches",
			steps: []step{
				{in: "\x1b[92;5", want: "", wantHeld: "\x1b[92;5"},
				{in: "u", want: "", wantDetach: true},
			},
		},
		{
			name: "the modifyOtherKeys encoding split across reads still detaches",
			steps: []step{
				{in: "\x1b[27;5;", want: "", wantHeld: "\x1b[27;5;"},
				{in: "92~", want: "", wantDetach: true},
			},
		},
		{
			// A held escape that turns out to be something else is released with what followed, in
			// order, so alt-x still reaches the program as ESC then x.
			name: "a held escape is released once it cannot be the key",
			steps: []step{
				{in: "\x1b", want: "", wantHeld: "\x1b"},
				{in: "x", want: "\x1bx"},
			},
		},
		{
			// Only the tail is held. Everything before it is not ambiguous and must not wait.
			name: "a read ending in escape forwards the rest at once",
			steps: []step{
				{in: "abc\x1b", want: "abc", wantHeld: "\x1b"},
			},
		},
		{
			// Alt-x arrives as one read, so nothing is ambiguous and nothing is held.
			name: "escape followed by a key in the same read is not held",
			steps: []step{
				{in: "\x1bx", want: "\x1bx"},
			},
		},
		{
			// An arrow key shares no suffix with the encodings, so it passes straight through. This
			// is the common case and the reason the holdback is cheap.
			name: "an ordinary escape sequence is not held",
			steps: []step{
				{in: "\x1b[A", want: "\x1b[A"},
			},
		},
		{
			name: "a partial that grows and then diverges is released in order",
			steps: []step{
				{in: "\x1b[9", want: "", wantHeld: "\x1b[9"},
				{in: "9", want: "\x1b[99"},
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			g := newGate(t, DefaultDetachKey)
			for i, s := range tc.steps {
				got, detach := g.feed([]byte(s.in), t0)
				if string(got) != s.want || detach != s.wantDetach {
					t.Fatalf("step %d: feed(%q) = (%q, %v), want (%q, %v)",
						i, s.in, got, detach, s.want, s.wantDetach)
				}
				if string(g.held) != s.wantHeld {
					t.Fatalf("step %d: feed(%q) left %q held, want %q", i, s.in, g.held, s.wantHeld)
				}
			}
		})
	}
}

func TestInputGateDeadline(t *testing.T) {
	g := newGate(t, DefaultDetachKey)

	// Nothing held, nothing to release.
	if deadline, holding := g.deadline(); holding {
		t.Errorf("deadline() = (%v, true) with nothing held, want holding false", deadline)
	}

	if _, _ = g.feed([]byte("\x1b"), t0); string(g.held) != "\x1b" {
		t.Fatalf("held = %q, want an escape", g.held)
	}
	deadline, holding := g.deadline()
	if !holding || !deadline.Equal(t0.Add(escapeGrace)) {
		t.Errorf("deadline() = (%v, %v), want (%v, true)", deadline, holding, t0.Add(escapeGrace))
	}

	// A later read that extends the partial keeps the original deadline. Restarting it would let a
	// stream that keeps ending in a partial postpone the release forever, which is the unbounded wait
	// this exists to remove.
	if _, _ = g.feed([]byte("[9"), t0.Add(30*time.Millisecond)); string(g.held) != "\x1b[9" {
		t.Fatalf("held = %q, want the grown partial", g.held)
	}
	deadline, holding = g.deadline()
	if !holding || !deadline.Equal(t0.Add(escapeGrace)) {
		t.Errorf("after a second read, deadline() = (%v, %v), want the original %v",
			deadline, holding, t0.Add(escapeGrace))
	}

	// Flushing releases the bytes and clears the deadline with them.
	if got := g.flush(); string(got) != "\x1b[9" {
		t.Errorf("flush() = %q, want the held bytes", got)
	}
	if _, holding := g.deadline(); holding {
		t.Error("deadline() reports holding after a flush")
	}
	if got := g.flush(); got != nil {
		t.Errorf("flush() = %q with nothing held, want nil", got)
	}
}

// While a nested client is attached inside the session, the key belongs to it: this gate must forward
// every encoding of it and withhold nothing, because the inner gate is what recognizes it and needs the
// whole sequence.
//
// The bug this is the unit for: the outer client always won, so ctrl-\ inside a nested attach detached the
// outer session, which for a per-window session closes the window.
func TestInputGateSuspendedForwardsTheKey(t *testing.T) {
	for _, in := range []string{"\x1c", "ls\x1c", "\x1b[92;5u", "\x1b[27;5;92~", "\x1b"} {
		g := newGate(t, DefaultDetachKey)
		g.suspended = true

		got, detach := g.feed([]byte(in), t0)
		if string(got) != in || detach {
			t.Errorf("feed(%q) = (%q, %v), want (%q, false)", in, got, detach, in)
		}
		if _, holding := g.deadline(); holding {
			t.Errorf("feed(%q) withheld bytes while suspended", in)
		}
	}
}

// The handover has to be reversible, and anything held when it happens is released in order rather than
// dropped: a nested attach that starts while an escape is withheld must not swallow that escape.
func TestInputGateSuspendedAndResumed(t *testing.T) {
	g := newGate(t, DefaultDetachKey)

	// An escape arrives and is withheld, as it always is.
	if got, detach := g.feed([]byte("\x1b"), t0); string(got) != "" || detach {
		t.Fatalf("feed(escape) = (%q, %v), want (%q, false)", got, detach, "")
	}

	// The nested client attaches. The next read releases what was held, in order, and the detach key
	// among it.
	g.suspended = true
	if got, detach := g.feed([]byte("[92;5u"), t0); string(got) != "\x1b[92;5u" || detach {
		t.Fatalf("feed(the rest) = (%q, %v), want (%q, false)", got, detach, "\x1b[92;5u")
	}

	// The nested client leaves and this gate is the innermost again.
	g.suspended = false
	if got, detach := g.feed([]byte("\x1c"), t0); string(got) != "" || !detach {
		t.Fatalf("feed(ctrl-\\) after resuming = (%q, %v), want (%q, true)", got, detach, "")
	}
}

// A disabled key must hold nothing at all, since the whole point of "none" is that the key belongs to
// the program. Holding for it would add latency for no possible benefit.
func TestInputGateDisabledKeyHoldsNothing(t *testing.T) {
	g := newGate(t, "none")
	for _, in := range []string{"\x1b", "\x1c", "\x1b[92;5"} {
		got, detach := g.feed([]byte(in), t0)
		if string(got) != in || detach {
			t.Errorf("feed(%q) = (%q, %v), want (%q, false)", in, got, detach, in)
		}
		if _, holding := g.deadline(); holding {
			t.Errorf("feed(%q) withheld bytes with the key disabled", in)
		}
	}
}
