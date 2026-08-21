package input

import (
	"reflect"
	"slices"
	"testing"
)

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
		{
			name: "kitty keyboard flags reply", input: "\x1b[?0u",
			wantTyped: false, wantReply: true,
			explanation: "the '?' marker makes this the answer to CSI ? u rather than a keypress, and " +
				"claiming it as typing sent a whole batch of replies to the pty verbatim",
		},
		{
			name: "kitty keyboard keypress", input: "\x1b[97u",
			wantTyped: true, wantReply: false,
			explanation: "a keycode rather than the '?' marker, so this really is someone typing 'a'",
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

// A chunk of replies splits into the individual sequences it holds.
//
// A terminal answers several questions in one write, and each answer belongs to a different question, so
// the caller has to match them one at a time. Handing a whole chunk to a single outstanding request is the
// reported `gh pr create --web` corruption: a real kitty replied with the background colour and a cursor
// report concatenated, the colour matched the question cm had asked, and the cursor report reached the pty
// inside the same blob.
func TestSplitReplies(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		// The measured shape of the bug: termenv's OSC 11 probe and the CSI 6n sentinel behind it, answered
		// together in one write.
		{
			name:  "colour reply and cursor report",
			input: "\x1b]11;rgb:2828/2c2c/3434\x1b\\\x1b[3;1R",
			want:  []string{"\x1b]11;rgb:2828/2c2c/3434\x1b\\", "\x1b[3;1R"},
		},
		{
			name:  "single reply",
			input: "\x1b[12;34R",
			want:  []string{"\x1b[12;34R"},
		},
		{
			name:  "three replies",
			input: "\x1b[0n\x1b[?62;22c\x1b[12;34R",
			want:  []string{"\x1b[0n", "\x1b[?62;22c", "\x1b[12;34R"},
		},
		// Both terminators, since cm's own replies use BEL while a real kitty uses ST, so a chunk can mix
		// them and splitting on one alone would swallow the sequence after it.
		{
			name:  "BEL and ST terminated OSC together",
			input: "\x1b]11;rgb:0000/0000/0000\x07\x1b]10;rgb:ffff/ffff/ffff\x1b\\",
			want:  []string{"\x1b]11;rgb:0000/0000/0000\x07", "\x1b]10;rgb:ffff/ffff/ffff\x1b\\"},
		},

		// Nothing is returned unless the whole chunk is recognized replies, matching IsQueryReply. A chunk
		// with typing in it is forwarded whole rather than picked apart, because dropping the front of
		// something that turns out to be a keystroke is worse than forwarding a duplicate.
		{"typing", "hello", nil},
		{"reply then typing", "\x1b[0nx", nil},
		{"typing then reply", "x\x1b[0n", nil},
		{"mouse report", "\x1b[<0;10;20M", nil},
		{"reply then mouse", "\x1b[0n\x1b[<0;10;20M", nil},
		{"incomplete trailing reply", "\x1b[0n\x1b[", nil},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitReplies([]byte(tt.input))
			gotStr := make([]string, 0, len(got))
			for _, s := range got {
				gotStr = append(gotStr, string(s))
			}
			if !slices.Equal(gotStr, tt.want) {
				t.Errorf("SplitReplies(%q) = %q, want %q", tt.input, gotStr, tt.want)
			}
		})
	}
}

// Splitting a chunk must agree with IsQueryReply about whether it is one.
//
// The two are consulted in sequence on the same bytes: recvLoop asks IsQueryReply whether to route a chunk
// to the proxy at all, and the proxy then splits it. A chunk IsQueryReply accepts but SplitReplies rejects
// falls back to being matched whole, which is the behavior the split exists to remove, and the
// disagreement would be invisible.
func TestSplitRepliesAgreesWithIsQueryReply(t *testing.T) {
	inputs := []string{
		"\x1b]11;rgb:2828/2c2c/3434\x1b\\\x1b[3;1R",
		"\x1b[12;34R",
		"\x1b[0n\x1b[?62;22c",
		"\x1b]52;c;aGk=\x07",
		"\x1bP>|kitty(0.42.2)\x1b\\",
		"hello",
		"\x1b[<0;10;20M",
		"\x1b[0nx",
		"\x1b[",
		"",
	}

	for _, in := range inputs {
		t.Run(in, func(t *testing.T) {
			isReply := IsQueryReply([]byte(in))
			split := SplitReplies([]byte(in)) != nil
			if isReply != split {
				t.Errorf("for %q: IsQueryReply = %v but SplitReplies returning anything = %v.\n"+
					"These are consulted on the same bytes in sequence, so a disagreement means a chunk "+
					"routed to the proxy as a reply is then matched whole instead of per sequence.",
					in, isReply, split)
			}
		})
	}
}

// A program's whole probe batch, answered by a real terminal in one write, must route to the query
// proxy rather than to the pty.
//
// This is the reported yazi bug. yazi probes the terminal on startup with DECRQSS for the cursor style,
// DECRQM for mode 12, CSI ? u for the kitty keyboard flags, and DA1, sent as one write. A real kitty
// answers all four in a single write, and these are the bytes it sent, captured under a pty rather than
// guessed:
//
//	\x1bP1$r1 q\x1b\\\x1b[?12;0$y\x1b[?0u
//
// The trailing kitty flags reply was classified as typing, and service.go asks IsUserInput before
// IsQueryReply, so the entire blob was written to the pty as though someone had typed it. yazi saw "r q"
// from the DECRQSS reply, minus the "\x1bP1$" introducer it never received: 'r' opens the rename prompt
// and " q" lands in the box, which is the reported screenshot. On the first run the leaked 'q' quit yazi
// outright instead.
//
// Asserted per sequence as well as on the whole blob, because the blob passing while a part fails would
// mean the split hands the wrong bytes to a matched request.
func TestTerminalProbeBatchIsNotTyping(t *testing.T) {
	const batch = "\x1bP1$r1 q\x1b\\\x1b[?12;0$y\x1b[?0u"

	if IsUserInput([]byte(batch)) {
		t.Errorf("IsUserInput(%q) = true, so service.go writes the batch to the pty verbatim instead of "+
			"matching it against the questions cm asked. yazi reads the DECRQSS reply as the keystrokes "+
			"'r', ' ', 'q': rename, then \" q\" typed into the prompt.", batch)
	}
	if !IsQueryReply([]byte(batch)) {
		t.Errorf("IsQueryReply(%q) = false, so the batch is forwarded to the pty rather than routed to "+
			"the query proxy", batch)
	}

	want := []string{"\x1bP1$r1 q\x1b\\", "\x1b[?12;0$y", "\x1b[?0u"}
	var got []string
	for _, part := range SplitReplies([]byte(batch)) {
		got = append(got, string(part))
	}
	if !slices.Equal(got, want) {
		t.Errorf("SplitReplies(%q) = %q, want %q", batch, got, want)
	}
}

// A chunk holding a query reply next to bytes only the program should see splits into both, with each
// part routed on its own.
//
// This is the reported kitty graphics corruption. `kitten icat` probes the terminal with APC
// (ESC _ G ... ST), and a real kitty answered the graphics response together with an unsolicited DA1
// reply in one write. Captured from a real session rather than constructed:
//
//	\x1b_Gi=1;OK\x1b\\\x1b[?62;52;c
//
// IsQueryReply is all-or-nothing, so it rejected the blob, IsUserInput did not claim it either, and it
// fell through to the verbatim pty write. The tty then echoed it back in caret notation, which is the
// "=1;OK" and "/62;52;c" garbage reported beside the prompt.
//
// The graphics response must stay a non-reply, and that is the load-bearing half. cm asks no graphics
// query -- internal/query/query.go classifies no APC at all -- so routing the response to the query
// proxy would match no outstanding request and hit the unmatched-reply discard, and `icat`, which did
// ask, would never get its answer. Measured while designing this: recognizing APC in classifyReply
// alone makes IsQueryReply return true for an APC-only chunk, which is exactly that discard path.
func TestSplitInputRoutesMixedChunks(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []Part
	}{
		{
			name:  "graphics response then unsolicited DA1 reply",
			input: "\x1b_Gi=1;OK\x1b\\\x1b[?62;52;c",
			want: []Part{
				{Data: []byte("\x1b_Gi=1;OK\x1b\\")},
				{Data: []byte("\x1b[?62;52;c"), Reply: true},
			},
		},
		{
			name:  "reply first, then graphics response",
			input: "\x1b[?62;52;c\x1b_Gi=1;OK\x1b\\",
			want: []Part{
				{Data: []byte("\x1b[?62;52;c"), Reply: true},
				{Data: []byte("\x1b_Gi=1;OK\x1b\\")},
			},
		},
		{
			name:  "focus event beside a reply",
			input: "\x1b[I\x1b[12;34R",
			want: []Part{
				{Data: []byte("\x1b[I")},
				{Data: []byte("\x1b[12;34R"), Reply: true},
			},
		},
		{
			name:  "mouse report beside a reply",
			input: "\x1b[<0;10;20M\x1b[0n",
			want: []Part{
				{Data: []byte("\x1b[<0;10;20M")},
				{Data: []byte("\x1b[0n"), Reply: true},
			},
		},
		{
			name:  "a graphics error response is still not a reply",
			input: "\x1b_Gi=3;EBADF:Bad file descriptor\x1b\\",
			want:  []Part{{Data: []byte("\x1b_Gi=3;EBADF:Bad file descriptor\x1b\\")}},
		},

		// Unrecognized or incomplete content forwards whole, which is the existing conservative rule:
		// dropping the front of something that turns out to be a keystroke is worse than a duplicate.
		{"typing", "hello", nil},
		{"reply then typing", "\x1b[0nx", nil},
		{"incomplete graphics response", "\x1b_Gi=1;OK", nil},
		{"empty", "", nil},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := SplitInput([]byte(tt.input))
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("SplitInput(%q) = %+v, want %+v", tt.input, got, tt.want)
			}
		})
	}
}
