package vt

import (
	"testing"
)

// The emulator answers some terminal queries and not others, and which ones matter to the server:
// see Session.drainPending, which decides whether to deliver those answers based on whether an
// attached client will answer instead.
//
// Recorded as a test because the set is a property of libghostty rather than of cm, so an upgrade can
// change it silently. Neither direction of change is harmless. A query that starts being answered
// becomes a reply cm may inject into the pty, which is the class of bug that printed "62;52;c" and
// ";rgb:2828/2c2c/3434" beside a prompt. A query that stops being answered leaves a detached session
// with nothing to answer it, and the failure there is a program hanging rather than stray output.
//
// Asserted against the live emulator instead of a table restating cm's assumptions, so it fails when
// libghostty's behavior moves rather than when someone edits a list to match.
func TestEmulatorAnsweredQueries(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		// want is whether writing seq makes the emulator generate bytes for the pty.
		want bool
	}{
		// Answered. cm is the only possible answerer for these when nothing is attached.
		{"DA1", "\x1b[c", true},
		{"DA1 explicit zero", "\x1b[0c", true},
		{"DA2", "\x1b[>c", true},
		{"DA3", "\x1b[=c", true},
		{"DECID", "\x1bZ", true},
		{"DSR status", "\x1b[5n", true},
		{"DSR cursor position", "\x1b[6n", true},
		{"XTVERSION", "\x1b[>q", true},
		{"kitty keyboard", "\x1b[?u", true},
		{"DECRQM sync output", "\x1b[?2026$p", true},
		{"DECRQM alt screen", "\x1b[?1049$p", true},

		// Not answered, and must not be: only the real terminal knows a background color, the
		// clipboard contents, or a window's pixel size. `wallfacer -h` blocks on the OSC 11 one, so a
		// change here would be a hang.
		{"OSC 10 foreground", "\x1b]10;?\x07", false},
		{"OSC 11 background", "\x1b]11;?\x07", false},
		{"OSC 52 clipboard read", "\x1b]52;c;?\x07", false},
		{"XTGETTCAP", "\x1bP+q544e\x1b\\", false},
		{"XTWINOPS pixel size", "\x1b[14t", false},
		{"XTWINOPS char size", "\x1b[18t", false},
		{"ANSI DECRQM", "\x1b[4$p", false},
		{"color scheme", "\x1b[?996n", false},

		// Ordinary output, which must not produce a reply at all.
		{"cursor move", "\x1b[10;20H", false},
		{"SGR", "\x1b[1;32m", false},
		{"cursor style", "\x1b[2 q", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := len(emulatorReplies(t, tc.seq)) > 0
			if got != tc.want {
				if tc.want {
					t.Errorf("the emulator no longer answers %q.\n"+
						"A detached session has no other answerer, so a program sending this now waits for "+
						"a reply that never comes. Check what changed in libghostty before editing this "+
						"expectation.", tc.seq)
				} else {
					t.Errorf("the emulator now answers %q, which it did not before.\n"+
						"drainPending may inject this reply into the pty, where it can be consumed by an "+
						"unrelated program's read. Confirm the gate in drainPending still covers it.", tc.seq)
				}
			}
		})
	}
}

// libghostty answers two sequences that are replies rather than requests, and answers each with its
// own bytes, making them fixpoints.
//
// Recorded because it bounds what the drainPending gate protects. A reply reaches the emulator
// whenever a shell echoes one at a prompt; the emulator answers it; that answer is written to the pty
// and can be echoed again. Nothing converges, since each turn produces identical bytes. The gate
// stops this whenever a client is attached, which is the interactive case where echo happens, but a
// detached session feeding itself its own DA2 reply would still loop.
//
// This failing is good news: it means libghostty tightened its matching and the hazard is gone.
func TestEmulatorAnswersItsOwnReplies(t *testing.T) {
	for _, seq := range []string{
		"\x1b[>1;0;0c", // DA2 reply
		"\x1b[?0u",     // kitty keyboard reply
	} {
		t.Run(seq, func(t *testing.T) {
			replies := emulatorReplies(t, seq)
			if len(replies) == 0 {
				t.Skipf("libghostty no longer answers %q, so the fixpoint is gone", seq)
			}
			if got := string(replies[0]); got != seq {
				t.Errorf("the emulator answered %q with %q, want identical bytes.\n"+
					"This documents a fixpoint; a different answer means the loop analysis above needs "+
					"revisiting.", seq, got)
			}
		})
	}
}

// emulatorReplies returns what libghostty generated for the pty in response to seq.
func emulatorReplies(t *testing.T, seq string) [][]byte {
	t.Helper()

	var replies [][]byte
	term, err := New(24, 80, Callbacks{
		WritePty: func(data []byte) { replies = append(replies, append([]byte(nil), data...)) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	defer term.Close()

	if err := term.Write([]byte(seq)); err != nil {
		t.Fatalf("Write(%q) error = %v", seq, err)
	}
	return replies
}
