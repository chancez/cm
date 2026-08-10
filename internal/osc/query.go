package osc

import "bytes"

// MaxQueryPartial bounds how many trailing bytes StripAnsweredQueries retains while waiting for an
// unterminated sequence to finish.
//
// Bounded for the same reason CommandTracker's maxPartial is: a stream that emits an ESC and then
// megabytes of ordinary text would otherwise grow the held buffer without limit. Every sequence
// recognized here is well under 32 bytes, so this is far more than enough.
const MaxQueryPartial = 64

// StripAnsweredQueries removes terminal queries that cm's own emulator answers, so they never reach
// the real terminal a client is attached to.
//
// The bug this fixes is visible and was reported as "vim leaves garbage below my prompt". A program
// asks the terminal a question, cm's emulator answers it, and the query *also* travelled out to the
// attached terminal, which answered too. The program reads the first reply and exits, so the second
// arrives with nobody waiting for it and the shell's line editor prints it at the prompt. The symptom
// was a fragment like "62;52;c" beside a zsh prompt, which is a DA1 reply with its ESC [ consumed:
// 52 is kitty's clipboard feature code, so the surviving copy was demonstrably the outer terminal's,
// not cm's. Stripping here means exactly one answer exists.
//
// Not merely cosmetic for cursor position reports. CSI 6n must be answered from cm's model, whose
// coordinates are the pty's. The outer terminal's coordinates describe its own window, including
// whatever surrounds the session, so an answer from there is in the wrong space. Before this, 6n was
// answered twice and the outer terminal's wrong answer arrived second.
//
// Only queries the emulator actually answers are removed. A query cm cannot answer must still reach
// the terminal, because only the terminal knows: OSC 10 and 11 report real colors, OSC 52 reads the
// real clipboard, and XTWINOPS reports real pixel dimensions. Removing one of those would replace a
// visible artifact with a hang, which is worse. The set here was established by probing the emulator
// rather than read off a specification, and vt.AnsweredQueries is the same list with a test that
// fails if libghostty's behavior drifts from it.
//
// Returns p unchanged when it holds no queries, which is almost every chunk, so this is cheap enough
// to run on the latency-sensitive path. held is any trailing partial sequence, which the caller must
// prepend to the next chunk: a query split across two reads would otherwise be reassembled by the
// terminal and answered after all, which is the same bug arriving intermittently.
func StripAnsweredQueries(p []byte) (out, held []byte) {
	// Fast path. A chunk with no ESC cannot contain a query, and ordinary output is the common case.
	if bytes.IndexByte(p, 0x1b) < 0 {
		return p, nil
	}

	var buf []byte
	// copied tracks whether buf holds a rewritten copy. Until something is actually stripped, p is
	// returned as-is rather than duplicated.
	copied := false
	i := 0
	start := 0

	for i < len(p) {
		if p[i] != 0x1b {
			i++
			continue
		}

		n, answered := classifyQuery(p[i:])
		if n == 0 {
			// An incomplete sequence at the end of the chunk, held so the rest can be matched against
			// it. A query split across two reads would otherwise be reassembled by the real terminal
			// and answered after all, which is this bug returning intermittently.
			//
			// Holding delays nothing observable, which is what makes this safe. An incomplete escape
			// sequence is not something the outer terminal can act on either: its own parser holds the
			// fragment until the final byte arrives, so buffering here has the same effect as
			// buffering one layer up. That is the difference from the detach-key holdback at
			// internal/client/detachkey.go, which retained bytes from a *complete* reply and so cut a
			// conversation short, leaving ";rgb:2828/2c2c/3434" on screen.
			//
			// Bounded because an OSC 52 clipboard payload runs to kilobytes and a stream could emit an
			// ESC followed by megabytes of text. Past the bound the fragment is forwarded and the
			// terminal reassembles it, since a long sequence is not one of the short queries stripped
			// here.
			if len(p)-i > MaxQueryPartial {
				break
			}
			buf = append(buf, p[start:i]...)
			return buf, p[i:]
		}

		if answered {
			// Drop the query by copying everything before it and resuming after it.
			buf = append(buf, p[start:i]...)
			copied = true
			i += n
			start = i
			continue
		}

		i += n
	}

	if !copied {
		return p, nil
	}
	return append(buf, p[start:]...), nil
}

// classifyQuery consumes one escape sequence, reporting its length and whether cm's emulator answers
// it.
//
// A length of zero means the sequence is incomplete within p.
func classifyQuery(p []byte) (n int, answered bool) {
	if len(p) < 2 {
		return 0, false
	}

	switch p[1] {
	case '[':
		return classifyCSIQuery(p)
	case 'Z':
		// DECID, the obsolete spelling of DA1. libghostty answers it identically, so it leaks the
		// same duplicate reply.
		return 2, true
	case ']':
		// OSC. Every OSC query cm sees is one only the real terminal can answer, such as OSC 10 and
		// 11 for colors or OSC 52 for the clipboard, so these pass through. Consumed rather than
		// skipped so an ESC inside a string terminator is not re-examined as a new sequence.
		if end := consumeStringSeq(p); end > 0 {
			return end, false
		}
		return 0, false
	case 'P', '_', '^':
		// DCS, APC, PM. XTGETTCAP arrives as DCS and the emulator does not answer it.
		if end := consumeStringSeq(p); end > 0 {
			return end, false
		}
		return 0, false
	default:
		return 2, false
	}
}

// classifyCSIQuery consumes a CSI sequence and decides whether the emulator answers it.
func classifyCSIQuery(p []byte) (n int, answered bool) {
	i := 2
	for i < len(p) && !isCSIFinalByte(p[i]) {
		i++
	}
	if i >= len(p) {
		return 0, false
	}
	final := p[i]
	params := p[2:i]
	length := i + 1

	// A reply is not a request, and telling them apart matters because a reply reaches this code
	// whenever the shell echoes it back. Both DA2's reply (CSI > 1 ; 0 ; 0 c) and the kitty keyboard
	// reply (CSI ? 0 u) re-trigger an answer if fed to the emulator, verified by probing it, so
	// treating one as a query here would strip legitimate output. Every reply carries a private
	// marker or parameters that a query does not, so the rules below are written to match the query
	// forms exactly rather than the family.
	switch final {
	case 'c':
		// DA1 is CSI c or CSI 0 c. DA2 is CSI > c or CSI > 0 c. DA3 is CSI = c or CSI = 0 c.
		// A DA1 reply is CSI ? ... c and a DA2 reply is CSI > n ; n ; n c, so a '?' prefix is never a
		// query and a '>' prefix is one only when nothing follows but an optional zero.
		switch {
		case len(params) == 0, bytes.Equal(params, []byte("0")):
			return length, true
		case params[0] == '>' || params[0] == '=':
			rest := params[1:]
			if len(rest) == 0 || bytes.Equal(rest, []byte("0")) {
				return length, true
			}
			return length, false
		default:
			return length, false
		}

	case 'n':
		// DSR. The emulator answers 5n (status) and 6n (cursor position). A '?' prefix is a reply or
		// an extension it does not answer, such as the color scheme query.
		if bytes.Equal(params, []byte("5")) || bytes.Equal(params, []byte("6")) {
			return length, true
		}
		return length, false

	case 'q':
		// XTVERSION is CSI > q, optionally with a zero. Other 'q' finals are cursor style and
		// DECSCA, which are not queries.
		if len(params) > 0 && params[0] == '>' {
			rest := params[1:]
			if len(rest) == 0 || bytes.Equal(rest, []byte("0")) {
				return length, true
			}
		}
		return length, false

	case 'u':
		// Kitty keyboard protocol query, CSI ? u. The reply is CSI ? <flags> u, which also triggers
		// an answer, so only the bare form counts as a query.
		if bytes.Equal(params, []byte("?")) {
			return length, true
		}
		return length, false

	case 'p':
		// DECRQM, CSI ? <mode> $ p for private modes and CSI <mode> $ p for ANSI ones. The emulator
		// answers the private form. The reply ends in $ y rather than $ p, so it cannot be confused
		// with a request.
		if len(params) > 1 && params[0] == '?' && params[len(params)-1] == '$' {
			return length, true
		}
		return length, false
	}

	return length, false
}

// isCSIFinalByte reports whether b terminates a CSI sequence.
func isCSIFinalByte(b byte) bool {
	return b >= 0x40 && b <= 0x7e
}

// consumeStringSeq consumes an OSC, DCS, APC, or PM sequence, returning its length.
//
// Returns 0 when it is not terminated within p. Both BEL and ST terminate one, and shells use both.
func consumeStringSeq(p []byte) int {
	for i := 2; i < len(p); i++ {
		if p[i] == 0x07 {
			return i + 1
		}
		if p[i] == 0x1b && i+1 < len(p) && p[i+1] == '\\' {
			return i + 2
		}
	}
	return 0
}
