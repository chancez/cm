//go:build cgo

package vt

import (
	"bytes"
	"testing"
)

// kittyFlagsReply returns what a terminal answers to CSI ? u, which is what a program inside a
// session receives when it asks which kitty keyboard flags are in effect.
//
// Asked of the model rather than read out of the blob on purpose. The formatter is free to emit the
// state as CSI = u or CSI > u, and which one it picks is not what the bug was about: the question is
// whether a reattaching client's terminal ends up in the state the program believes it set.
func kittyFlagsReply(t *testing.T, term *Terminal, replies *[][]byte) string {
	t.Helper()
	*replies = nil
	if err := term.Write([]byte("\x1b[?u")); err != nil {
		t.Fatalf("Write(CSI ? u) error = %v", err)
	}
	if len(*replies) != 1 {
		t.Fatalf("asking CSI ? u produced %d replies, want exactly 1: %q", len(*replies), *replies)
	}
	return string((*replies)[0])
}

// newCapturingTerminal returns a terminal along with the replies it generates for the pty.
func newCapturingTerminal(t *testing.T, rows, cols uint16) (*Terminal, *[][]byte) {
	t.Helper()
	replies := new([][]byte)
	term, err := New(rows, cols, Callbacks{
		WritePty: func(d []byte) { *replies = append(*replies, append([]byte(nil), d...)) },
	})
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}
	t.Cleanup(func() { _ = term.Close() })
	return term, replies
}

// A restore has to carry the kitty keyboard protocol flags the program set.
//
// The bug: the blob is all a fresh client gets, and its terminal was just reset, so a blob that omits
// the flags leaves the terminal encoding keys the legacy way while the program still believes the
// flags it pushed are in effect. Claude Code pushes flag 1 at startup, measured in a sandbox as
// "\x1b[<u\x1b[>1u\x1b[>4;2m", so every reattach to a session running it desynchronized the two.
// Flag 1 is "disambiguate escape codes", which is what makes Escape arrive as CSI 27 u rather than a
// bare ESC, so the visible symptom is Escape and modified keys such as shift+tab going dead after a
// reattach while ordinary typing keeps working.
//
// The cause was one unset option: internal/vt set extra.keyboard, which covers xterm's
// modifyOtherKeys, and never set extra.screen.kitty_keyboard, which covers these. The two names are
// close enough that the missing one reads as already handled.
//
// Asserted by replaying the blob into a fresh terminal and asking it the same question the program
// asks, so the test is about the state a program observes rather than the bytes chosen to convey it.
func TestRestoreCarriesKittyKeyboardFlags(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup string
		want  string
	}{
		{
			// What Claude Code does at startup: pop whatever is there, then push flag 1.
			name:  "pushed flag 1, as Claude Code does",
			setup: "\x1b[<u\x1b[>1u",
			want:  "\x1b[?1u",
		},
		{
			name:  "set rather than pushed",
			setup: "\x1b[=13;1u",
			want:  "\x1b[?13u",
		},
		{
			// A full-screen program: the flags belong to the screen it is on, so a blob for an
			// alternate-screen session has to carry them too.
			name:  "pushed on the alternate screen",
			setup: "\x1b[?1049h\x1b[>15u",
			want:  "\x1b[?15u",
		},
		{
			// The control. Without it a fix that always emits some flags value would pass every
			// case above while corrupting the common session that never touched the protocol.
			name:  "never touched, so nothing is restored",
			setup: "",
			want:  "\x1b[?0u",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			src, srcReplies := newCapturingTerminal(t, 10, 40)
			if err := src.Write([]byte(tc.setup + "SESSION_TEXT")); err != nil {
				t.Fatalf("Write() error = %v", err)
			}
			if got := kittyFlagsReply(t, src, srcReplies); got != tc.want {
				t.Fatalf("the session's own flags = %q, want %q; the setup did not do what it claims",
					got, tc.want)
			}

			restore, err := src.Restore()
			if err != nil {
				t.Fatalf("Restore() error = %v", err)
			}

			// A fresh client: a terminal that has just been reset, which is all a reattach gives.
			dst, dstReplies := newCapturingTerminal(t, 10, 40)
			if err := dst.Write(restore); err != nil {
				t.Fatalf("replaying restore bytes: %v", err)
			}

			if got := kittyFlagsReply(t, dst, dstReplies); got != tc.want {
				t.Errorf("after replaying the restore blob the client answers CSI ? u with %q, want %q.\n"+
					"The program in the session still believes it set %q, so the terminal and the program "+
					"disagree about how keys are encoded: Escape and modified keys stop working.\n"+
					"blob = %q",
					got, tc.want, tc.want, restore)
			}
		})
	}
}

// modifyOtherKeys survives a restore too, and did before the flags fix.
//
// Pinned separately because it comes from a different formatter option, extra.keyboard, and the
// original diagnosis of the flags bug wrongly reported this as broken as well: the probe that found
// it never wrote CSI > 4 ; 2 m in the first place, so its absence from the blob proved nothing. A
// test naming both keeps the next reader from re-deriving which option covers which sequence.
func TestRestoreCarriesModifyOtherKeys(t *testing.T) {
	src, _ := newCapturingTerminal(t, 10, 40)
	if err := src.Write([]byte("\x1b[>4;2m" + "SESSION_TEXT")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}

	restore, err := src.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if !bytes.Contains(restore, []byte("\x1b[>4;2m")) {
		t.Errorf("restore blob = %q, want it to carry modifyOtherKeys (\\x1b[>4;2m)", restore)
	}

	// The control: a session that never set it must not have it invented.
	plain, _ := newCapturingTerminal(t, 10, 40)
	if err := plain.Write([]byte("SESSION_TEXT")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	blob, err := plain.Restore()
	if err != nil {
		t.Fatalf("Restore() error = %v", err)
	}
	if bytes.Contains(blob, []byte("\x1b[>4")) {
		t.Errorf("restore blob = %q, want no modifyOtherKeys for a session that never set it", blob)
	}
}
