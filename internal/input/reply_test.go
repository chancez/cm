package input

import "testing"

func TestIsQueryReply(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		// Answers to questions a program asked. Exactly one attached terminal may send these, so
		// these are the sequences that get dropped from every client but the answerer.
		{"DA1 reply", "\x1b[?62;22c", true},
		{"kitty DA1 reply", "\x1b[?62;52;c", true},
		{"DA2 reply", "\x1b[>1;4000;0c", true},
		{"DA3 reply", "\x1bP!|00000000\x1b\\", true},
		{"cursor position report", "\x1b[12;34R", true},
		{"device status report", "\x1b[0n", true},
		{"kitty keyboard flags", "\x1b[?0u", true},
		{"DECRPM", "\x1b[?2026;2$y", true},
		{"XTWINOPS size report", "\x1b[4;600;800t", true},
		{"XTVERSION reply", "\x1bP>|kitty(0.42.2)\x1b\\", true},
		{"OSC 11 background reply", "\x1b]11;rgb:2828/2c2c/3434\x1b\\", true},
		{"OSC 10 foreground reply", "\x1b]10;rgb:ffff/ffff/ffff\x07", true},
		{"OSC 52 clipboard contents", "\x1b]52;c;aGk=\x07", true},
		{"two replies in one chunk", "\x1b[0n\x1b[?62;22c", true},

		// Not replies, and must reach the shell from every client. A mouse or focus event describes
		// one window, so each attached terminal is entitled to send its own; dropping them would make
		// a session ignore the mouse in every window but the answerer's.
		{"SGR mouse press", "\x1b[<0;10;20M", false},
		{"SGR mouse release", "\x1b[<0;10;20m", false},
		{"X10 mouse", "\x1b[M\x20\x21\x22", false},
		{"focus in", "\x1b[I", false},
		{"focus out", "\x1b[O", false},

		// Typing, which must never be dropped.
		{"plain text", "hello", false},
		{"single letter", "a", false},
		{"control character", "\x03", false},
		{"return", "\r", false},
		{"arrow key", "\x1b[A", false},
		{"function key", "\x1b[15~", false},
		{"kitty keypress", "\x1b[97;5u", false},
		{"SS3 key", "\x1bOP", false},
		{"alt-modified key", "\x1bx", false},
		{"bare escape", "\x1b", false},

		// A request is not a reply, even though it shares a final byte. A client should not normally
		// send one, and treating it as a reply would drop it silently.
		{"DA1 request", "\x1b[c", false},
		{"DSR request", "\x1b[6n", false},

		// Mixed content is not purely a reply, so it is forwarded rather than risk dropping the
		// keystroke travelling with it.
		{"reply followed by typing", "\x1b[0nx", false},
		{"typing followed by reply", "x\x1b[0n", false},

		// Nothing is not a reply, so a caller never drops an empty write for the wrong reason.
		{"empty", "", false},

		// An incomplete sequence is forwarded. Guessing on a fragment risks dropping the front of
		// something that turns out to be typing.
		{"partial CSI", "\x1b[", false},
		{"partial OSC", "\x1b]11;rgb:2828", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsQueryReply([]byte(tt.input)); got != tt.want {
				t.Errorf("IsQueryReply(%q) = %v, want %v", tt.input, got, tt.want)
			}
		})
	}
}

// The two classifiers answer different questions, and the cases where they differ are the whole
// reason both exist. Asserted directly so a change that collapses them fails here.
func TestIsQueryReplyAndIsUserInputDisagreeWhereIntended(t *testing.T) {
	cases := []struct {
		name        string
		input       string
		wantTyped   bool
		wantReply   bool
		explanation string
	}{
		{
			name: "cursor position report", input: "\x1b[12;34R",
			wantTyped: false, wantReply: true,
			explanation: "not typing so it cannot claim sizing, and a reply so only one client sends it",
		},
		{
			name: "SGR mouse", input: "\x1b[<0;10;20M",
			wantTyped: false, wantReply: false,
			explanation: "not typing, but every window reports its own mouse so it must not be dropped",
		},
		{
			name: "focus in", input: "\x1b[I",
			wantTyped: false, wantReply: false,
			explanation: "not typing, but focus is per window so every client may report it",
		},
		{
			name: "typed letter", input: "a",
			wantTyped: true, wantReply: false,
			explanation: "typing, so it claims sizing and is never dropped",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			typed := IsUserInput([]byte(tc.input))
			reply := IsQueryReply([]byte(tc.input))
			if typed != tc.wantTyped || reply != tc.wantReply {
				t.Errorf("for %q: IsUserInput = %v (want %v), IsQueryReply = %v (want %v)\n%s",
					tc.input, typed, tc.wantTyped, reply, tc.wantReply, tc.explanation)
			}
		})
	}
}
