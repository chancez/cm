package vt

import (
	"testing"

	"github.com/chancez/cm/internal/osc"
)

// osc.StripAnsweredQueries removes terminal queries this emulator answers, and the two halves live in
// different packages: the matching is pure bytes in internal/osc, the answering is inside libghostty.
// Nothing links them, so an upgrade that starts or stops answering something would silently desync
// them, and both directions are bad. An answered query that still reaches the real terminal gets
// answered twice, which is the bug that printed "62;52;c" beside a zsh prompt. A query stripped that
// nothing answers reaches nobody, so the program that asked blocks forever, and a hang is worse than
// an artifact.
//
// Asserting against the live emulator rather than a hardcoded table is the point: a table would only
// restate what osc already believes and would pass while both were wrong together.
func TestStripMatchesWhatTheEmulatorAnswers(t *testing.T) {
	cases := []struct {
		name string
		seq  string
		// query means this is a request cm must answer alone, so it has to be stripped from output.
		query bool
		// overAnswered marks a sequence libghostty answers even though it is not a request. See
		// TestEmulatorOverAnswersReplies: these must not be stripped, because they are real output.
		overAnswered bool
	}{
		{name: "DA1", seq: "\x1b[c", query: true},
		{name: "DA1 explicit zero", seq: "\x1b[0c", query: true},
		{name: "DA2", seq: "\x1b[>c", query: true},
		{name: "DA2 explicit zero", seq: "\x1b[>0c", query: true},
		{name: "DA3", seq: "\x1b[=c", query: true},
		{name: "DECID", seq: "\x1bZ", query: true},
		{name: "DSR status", seq: "\x1b[5n", query: true},
		{name: "DSR cursor position", seq: "\x1b[6n", query: true},
		{name: "XTVERSION", seq: "\x1b[>q", query: true},
		{name: "XTVERSION explicit zero", seq: "\x1b[>0q", query: true},
		{name: "kitty keyboard", seq: "\x1b[?u", query: true},
		{name: "DECRQM sync output", seq: "\x1b[?2026$p", query: true},
		{name: "DECRQM alt screen", seq: "\x1b[?1049$p", query: true},
		{name: "DECRQM mouse", seq: "\x1b[?1000$p", query: true},

		// Queries only the real terminal can answer, so they must reach it. cm has no idea what the
		// real background color, clipboard contents, or pixel dimensions are.
		{name: "OSC 10 foreground", seq: "\x1b]10;?\x07"},
		{name: "OSC 11 background", seq: "\x1b]11;?\x07"},
		{name: "OSC 52 clipboard read", seq: "\x1b]52;c;?\x07"},
		{name: "XTGETTCAP", seq: "\x1bP+q544e\x1b\\"},
		{name: "XTWINOPS pixel size", seq: "\x1b[14t"},
		{name: "XTWINOPS char size", seq: "\x1b[18t"},
		{name: "XTWINOPS screen size", seq: "\x1b[16t"},
		{name: "ANSI DECRQM", seq: "\x1b[4$p"},
		{name: "color scheme", seq: "\x1b[?996n"},

		// Replies, which reach output whenever the shell echoes one back at a prompt. Stripping a
		// reply would delete real output. Two of them are answered by libghostty anyway.
		{name: "DA1 reply", seq: "\x1b[?62;22c"},
		{name: "kitty DA1 reply", seq: "\x1b[?62;52;c"},
		{name: "DA2 reply", seq: "\x1b[>1;0;0c", overAnswered: true},
		{name: "kitty keyboard reply", seq: "\x1b[?0u", overAnswered: true},
		{name: "DSR status reply", seq: "\x1b[0n"},
		{name: "cursor position reply", seq: "\x1b[12;34R"},
		{name: "DECRQM reply", seq: "\x1b[?2026;2$y"},

		// Ordinary output sharing a final byte with a query family.
		{name: "cursor move", seq: "\x1b[10;20H"},
		{name: "cursor style", seq: "\x1b[2 q"},
		{name: "SGR", seq: "\x1b[1;32m"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			out, held := osc.StripAnsweredQueries([]byte(tc.seq))
			stripped := len(out) == 0 && len(held) == 0
			answered := emulatorAnswers(t, tc.seq)

			// Every request cm answers must be stripped, and nothing else may be.
			if stripped != tc.query {
				if tc.query {
					t.Errorf("osc.StripAnsweredQueries(%q) left it as (%q, %q), want it removed.\n"+
						"A query cm answers that still reaches the real terminal is answered twice, and the "+
						"spare reply lands in the shell's line editor.", tc.seq, out, held)
				} else {
					t.Errorf("osc.StripAnsweredQueries(%q) removed it, want it passed through.\n"+
						"This is not a request cm answers: stripping it either deletes real output or "+
						"makes the program that asked block forever.", tc.seq)
				}
			}

			// And the emulator must actually answer what is being stripped on its behalf.
			if tc.query && !answered {
				t.Errorf("osc strips %q but the emulator no longer answers it.\n"+
					"Nothing will answer this query now, so a program asking it hangs. libghostty's "+
					"behavior changed: drop it from classifyCSIQuery.", tc.seq)
			}
			if !tc.query && answered && !tc.overAnswered {
				t.Errorf("the emulator answers %q, which osc does not strip.\n"+
					"That means it reaches the real terminal too and gets answered twice. Either add it "+
					"to classifyCSIQuery or mark it overAnswered with a reason.", tc.seq)
			}
		})
	}
}

// libghostty answers two sequences that are replies rather than requests, and its answer to each is
// byte-identical to the input. That makes them fixpoints, which is why they are called out rather
// than folded into the table above.
//
// The risk is a feedback loop, not a wrong reply. A reply reaches output when the shell echoes it at a
// prompt; the emulator answers it; drainPending writes that answer to the pty; the shell echoes it
// again. Each turn produces the same bytes, so nothing converges. cm does not strip these, precisely
// because they are output a client must see, so the loop is bounded only by the shell not echoing.
//
// Recorded as a test so a libghostty upgrade that tightens the matching is noticed: this failing is
// good news, and the fix is to drop the overAnswered marks above.
func TestEmulatorOverAnswersReplies(t *testing.T) {
	fixpoints := []string{
		"\x1b[>1;0;0c", // DA2 reply
		"\x1b[?0u",     // kitty keyboard reply
	}

	for _, seq := range fixpoints {
		t.Run(seq, func(t *testing.T) {
			replies := emulatorReplies(t, seq)
			if len(replies) == 0 {
				t.Skipf("libghostty no longer answers %q, so the fixpoint is gone. Drop the "+
					"overAnswered mark for it in TestStripMatchesWhatTheEmulatorAnswers.", seq)
			}
			if got := string(replies[0]); got != seq {
				t.Errorf("the emulator answered %q with %q, want the same bytes back.\n"+
					"This test exists to document a fixpoint; if the answer differs the loop analysis "+
					"in the comment above needs revisiting.", seq, got)
			}
		})
	}
}

// emulatorAnswers reports whether writing seq makes libghostty generate a reply for the pty.
func emulatorAnswers(t *testing.T, seq string) bool {
	t.Helper()
	return len(emulatorReplies(t, seq)) > 0
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
