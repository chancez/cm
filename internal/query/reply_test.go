package query

import "testing"

func TestAnswersQuery(t *testing.T) {
	tests := []struct {
		name  string
		query string
		reply string
		want  bool
	}{
		// The colour queries, answered by an OSC carrying the same code. Both terminators appear in
		// practice, so neither can be assumed: cm's own replies use BEL and a real kitty uses ST.
		{"OSC 11 answered by OSC 11", "\x1b]11;?\x1b\\", "\x1b]11;rgb:2828/2c2c/3434\x1b\\", true},
		{"OSC 11 answered with BEL", "\x1b]11;?\x1b\\", "\x1b]11;rgb:2828/2c2c/3434\x07", true},
		{"OSC 10 answered by OSC 10", "\x1b]10;?\x07", "\x1b]10;rgb:ffff/ffff/ffff\x07", true},
		{"OSC 12 answered by OSC 12", "\x1b]12;?\x07", "\x1b]12;rgb:ffff/ffff/ffff\x07", true},
		{"OSC 52 clipboard read answered", "\x1b]52;c;?\x07", "\x1b]52;c;aGk=\x07", true},

		// The reported bug. termenv sends OSC 11 and then CSI 6n as a sentinel; cm answers the CSI 6n
		// itself and forwards it to the client too, so the client's terminal answers it as well. That
		// cursor report is not an answer to the colour question, and accepting it printed
		// "^[[42;1R^[[42;1R" beside the prompt.
		{"OSC 11 not answered by a cursor report", "\x1b]11;?\x1b\\", "\x1b[42;1R", false},
		{"OSC 11 not answered by a DA1 reply", "\x1b]11;?\x1b\\", "\x1b[?62;22c", false},
		{"OSC 11 not answered by a DSR", "\x1b]11;?\x1b\\", "\x1b[0n", false},

		// A different OSC code is a different question. Some prompt hooks ask several at once, and
		// accepting any OSC would answer the foreground with the background.
		{"OSC 11 not answered by OSC 10", "\x1b]11;?\x1b\\", "\x1b]10;rgb:ffff/ffff/ffff\x07", false},
		{"OSC 52 not answered by OSC 11", "\x1b]52;c;?\x07", "\x1b]11;rgb:2828/2c2c/3434\x07", false},

		// XTWINOPS size reports, whose reply code differs from the request's: 14 t is answered by 4 t,
		// 16 t by 6 t, 18 t by 8 t. Matching any CSI t would let one size answer another.
		{"CSI 14 t answered by 4 t", "\x1b[14t", "\x1b[4;600;800t", true},
		{"CSI 16 t answered by 6 t", "\x1b[16t", "\x1b[6;20;10t", true},
		{"CSI 18 t answered by 8 t", "\x1b[18t", "\x1b[8;24;80t", true},
		{"CSI 14 t not answered by 8 t", "\x1b[14t", "\x1b[8;24;80t", false},
		{"CSI 18 t not answered by a cursor report", "\x1b[18t", "\x1b[42;1R", false},

		// XTGETTCAP, whose reply is a DCS either way: DCS 1 + r when the capability is known and
		// DCS 0 + r when it is not. A "not found" is a real answer and must release the request, or
		// every query behind it waits out the timeout.
		{"XTGETTCAP answered", "\x1bP+q544e\x1b\\", "\x1bP1+r544e=787465726d\x1b\\", true},
		{"XTGETTCAP answered by a not-found", "\x1bP+q7a7a\x1b\\", "\x1bP0+r7a7a\x1b\\", true},
		{"XTGETTCAP not answered by a cursor report", "\x1bP+q544e\x1b\\", "\x1b[42;1R", false},

		// Malformed and empty input matches nothing, so a request is left to expire rather than being
		// consumed by bytes that answer no question.
		{"empty reply", "\x1b]11;?\x1b\\", "", false},
		{"empty query", "", "\x1b[42;1R", false},
		{"unterminated reply", "\x1b]11;?\x1b\\", "\x1b]11;rgb", false},
		{"plain text reply", "\x1b]11;?\x1b\\", "hello", false},
		{"OSC with no code", "\x1b];?\x07", "\x1b];x\x07", false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := AnswersQuery([]byte(tc.query), []byte(tc.reply)); got != tc.want {
				t.Errorf("AnswersQuery(%q, %q) = %v, want %v", tc.query, tc.reply, got, tc.want)
			}
		})
	}
}

// Every query cm proxies must have a reply shape AnswersQuery recognizes.
//
// The two halves have to stay in step, and this is the direction that fails silently: adding a query to
// the terminal-only set without teaching AnswersQuery its reply means cm asks the question, the client
// answers it correctly, and the answer is discarded as unrecognized. The program then waits out the
// timeout for an answer its terminal already gave, which reads as cm being slow rather than as a missing
// case here.
func TestEveryProxiedQueryHasARecognizedReply(t *testing.T) {
	cases := []struct {
		query string
		reply string
	}{
		{"\x1b]10;?\x07", "\x1b]10;rgb:ffff/ffff/ffff\x07"},
		{"\x1b]11;?\x07", "\x1b]11;rgb:2828/2c2c/3434\x07"},
		{"\x1b]12;?\x07", "\x1b]12;rgb:ffff/ffff/ffff\x07"},
		{"\x1b]17;?\x07", "\x1b]17;rgb:4444/4444/4444\x07"},
		{"\x1b]19;?\x07", "\x1b]19;rgb:0000/0000/0000\x07"},
		{"\x1b]52;c;?\x07", "\x1b]52;c;aGk=\x07"},
		{"\x1b[14t", "\x1b[4;600;800t"},
		{"\x1b[16t", "\x1b[6;20;10t"},
		{"\x1b[18t", "\x1b[8;24;80t"},
		{"\x1bP+q544e\x1b\\", "\x1bP1+r544e=787465726d\x1b\\"},
	}

	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			// The premise: this is a query cm proxies. A failure here means the case list has drifted
			// from the classifier rather than that AnswersQuery is wrong.
			if !IsTerminalOnlyRequest([]byte(tc.query)) {
				t.Fatalf("IsTerminalOnlyRequest(%q) = false, want true: this list must hold only "+
					"proxied queries", tc.query)
			}
			if !AnswersQuery([]byte(tc.query), []byte(tc.reply)) {
				t.Errorf("AnswersQuery(%q, %q) = false, want true.\n"+
					"cm proxies this query, so a client's reply to it must be recognized. Unrecognized "+
					"means the answer is discarded and the asking program waits out requestTimeout for "+
					"a reply its terminal already sent.", tc.query, tc.reply)
			}
		})
	}
}
