// Package input classifies bytes arriving from a client.
//
// It exists to answer one question: did a person type this? That matters because cm uses it to
// decide which of several attached clients owns the terminal's size, and the wrong answer is
// visible. A terminal sends far more than keystrokes on the input channel: mouse motion, focus
// changes, and answers to questions the program asked. Treating any of those as typing lets a window
// nobody touched take over sizing, so a session reflows because a mouse crossed it.
//
// zmx got this wrong twice, once where a device status report claimed ownership and once where
// injected input did, so the cases below are enumerated rather than inferred.
package input

import "bytes"

// IsUserInput reports whether p contains something a person typed.
//
// Conservative in the direction that matters: an unrecognized escape sequence is treated as *not*
// typing. A missed keystroke costs one more keypress before sizing transfers, while a false positive
// reflows a window for no reason.
func IsUserInput(p []byte) bool {
	for i := 0; i < len(p); {
		b := p[i]

		if b == 0x1b {
			n, typed := classifyEscape(p[i:])
			if typed {
				return true
			}
			if n <= 0 {
				// An incomplete sequence at the end of the buffer. The rest arrives next time, and
				// guessing now could only produce a false positive.
				return false
			}
			i += n
			continue
		}

		// A control character is typed: Ctrl-C, Return, Tab, and Backspace all arrive this way, and
		// all of them mean someone is using this window.
		if b < 0x20 || b == 0x7f {
			return true
		}

		// Anything printable, including the continuation bytes of a multi-byte rune.
		return true
	}
	return false
}

// IsQueryReply reports whether p is entirely answers to questions a program asked the terminal.
//
// Separate from IsUserInput because the two decide different things and want different answers for
// the same bytes. IsUserInput asks "may this claim sizing", where a mouse report and a device status
// report are alike in saying no. This asks "is this an answer only one terminal should send", where
// they differ: several attached terminals must not each answer one query, but each may report its own
// mouse and focus events, since those describe that window rather than the session.
//
// Conservative in the opposite direction from IsUserInput, and for the same underlying reason. Only
// sequences recognized as replies count, so an unrecognized one is forwarded rather than dropped. A
// forwarded duplicate is the visible artifact this exists to reduce; a dropped keystroke or a dropped
// mouse event is a session that ignores input, which is worse.
//
// Empty input is not a reply, so a caller never drops nothing for the wrong reason.
func IsQueryReply(p []byte) bool {
	if len(p) == 0 {
		return false
	}
	for i := 0; i < len(p); {
		if p[i] != 0x1b {
			// Anything typed or printable means this is not purely a reply.
			return false
		}
		n, reply := classifyReply(p[i:])
		if n <= 0 || !reply {
			return false
		}
		i += n
	}
	return true
}

// SplitReplies breaks a chunk of replies into the individual sequences it holds.
//
// A terminal answers several questions in one write, so the bytes arriving here are routinely more than
// one reply, and each has to be matched to its own question. Handing the whole chunk to a single
// outstanding request was the second half of the reported `gh pr create --web` corruption: cm asked the
// background colour, and the client's terminal replied with the colour *and* a cursor report in one write,
// because it had also seen the CSI 6n cm forwards to every client. The colour matched, so the whole chunk
// including the cursor report was written to the pty as the answer.
//
// Returns nil unless the chunk is entirely recognized replies, which is the same conservative rule
// IsQueryReply applies and for the same reason: a chunk with anything unrecognized in it is forwarded
// whole rather than picked apart, since dropping the front of something that turns out to be typing is
// worse than forwarding a duplicate.
func SplitReplies(p []byte) [][]byte {
	var out [][]byte
	for i := 0; i < len(p); {
		if p[i] != 0x1b {
			return nil
		}
		n, reply := classifyReply(p[i:])
		if n <= 0 || !reply {
			return nil
		}
		out = append(out, p[i:i+n])
		i += n
	}
	return out
}

// Part is one piece of a client's input chunk, and where it has to go.
//
// Three destinations rather than two, which is the distinction SplitInput exists to make. A chunk can
// hold answers to questions cm asked, bytes only the program should see, or both at once, and the
// difference decides whether a sequence may be matched against an outstanding request or must be
// written to the pty untouched.
type Part struct {
	// Data is the sequence itself.
	Data []byte
	// Reply is set when this sequence answers a query cm asked a client, so the caller may match it
	// against an outstanding request and drop it when it matches nothing.
	//
	// False does not mean "typing": a mouse report, a focus event, and a kitty graphics response are
	// all not-replies that must still reach the program.
	Reply bool
}

// ReplyFramer holds an incomplete terminal reply until its remaining bytes arrive.
//
// A pty read is capped at 1022 bytes on macOS, while an OSC 52 clipboard response can be much
// larger. Classifying each read independently sends the unterminated head to the program and then
// mistakes the continuation for typing. The buffer belongs to one client attachment: replies from
// different terminals must never be joined.
//
// Only string controls are held. A CSI reply is short, while holding a partial CSI would delay an
// ordinary escape-prefixed keypress. OSC, DCS, and APC are the reply forms whose payload can be large.
type ReplyFramer struct {
	partial []byte
}

// Split returns complete input parts from p and holds an incomplete string-control reply for the next
// call. A reply part is kept whole so the server can match it to the query it sent; all other bytes stay
// in order as ordinary input.
func (f *ReplyFramer) Split(p []byte) []Part {
	if len(f.partial) > 0 {
		p = append(f.partial, p...)
		f.partial = nil
	}

	var out []Part
	for len(p) > 0 {
		if p[0] != 0x1b {
			i := 1
			for i < len(p) && p[i] != 0x1b {
				i++
			}
			out = appendInputPart(out, p[:i])
			p = p[i:]
			continue
		}

		n, reply := classifyReply(p)
		if n > 0 {
			if reply {
				out = append(out, Part{Data: p[:n], Reply: true})
			} else {
				out = appendInputPart(out, p[:n])
			}
			p = p[n:]
			continue
		}

		if beginsStringControl(p) {
			f.partial = append(f.partial, p...)
			break
		}

		// A bare escape or incomplete CSI is a keypress, not a large reply. Passing it through keeps
		// escape responsive rather than waiting for a later read that may never happen.
		out = appendInputPart(out, p[:1])
		p = p[1:]
	}
	return out
}

func beginsStringControl(p []byte) bool {
	return len(p) >= 2 && p[0] == 0x1b && (p[1] == ']' || p[1] == 'P' || p[1] == '_')
}

func appendInputPart(parts []Part, data []byte) []Part {
	if len(data) == 0 {
		return parts
	}
	if n := len(parts); n > 0 && !parts[n-1].Reply {
		parts[n-1].Data = append(parts[n-1].Data, data...)
		return parts
	}
	return append(parts, Part{Data: data})
}

// SplitInput breaks a client's input chunk into its sequences and says where each one goes.
//
// This exists because IsQueryReply is all-or-nothing, and a real terminal does not write chunks that
// way. Reported against kitty graphics: `kitten icat` probes with APC (`ESC _ G ... ST`) and the
// terminal answered a graphics response and an unsolicited DA1 reply in one write, measured as
// "\x1b_Gi=1;OK\x1b\\\x1b[?62;52;c". Neither IsUserInput nor IsQueryReply claimed the blob, so it went
// to the pty verbatim and the tty echoed the whole thing back in caret notation, which is the reported
// "=3;EBADF:...=1;OK" and "/62;52;c" garbage beside the prompt.
//
// The important half of that is *why* a graphics response must not be treated as a reply. cm never asks
// a graphics query: internal/query/query.go classifies no APC at all, so an icat response matches no
// outstanding request, and routing it to the query proxy would hit the unmatched-reply discard that
// exists for the git-branch-into-a-prompt bug. The program asked, so the program must receive it. That
// makes the naive "recognize APC as a reply" fix actively wrong, and it was measured: adding APC to
// classifyReply alone makes IsQueryReply return true for an APC-only chunk, which is the discard path.
//
// Returns nil when the chunk holds anything unrecognized, which keeps the existing conservative rule:
// a chunk that does not parse cleanly is forwarded whole rather than picked apart, since dropping the
// front of something that turns out to be a keystroke is worse than forwarding a duplicate.
func SplitInput(p []byte) []Part {
	var out []Part
	for i := 0; i < len(p); {
		if p[i] != 0x1b {
			return nil
		}
		n, reply := classifyReply(p[i:])
		if n > 0 && reply {
			out = append(out, Part{Data: p[i : i+n], Reply: true})
			i += n
			continue
		}
		// Not a reply. Recognized non-reply sequences still have to reach the program, so they are
		// carried as parts rather than abandoning the split: that is the whole point, since giving up
		// here is what let a reply ride to the pty inside its neighbour.
		if n = passthroughLen(p[i:]); n > 0 {
			out = append(out, Part{Data: p[i : i+n]})
			i += n
			continue
		}
		return nil
	}
	return out
}

// passthroughLen reports the length of a sequence that is not a reply but must still reach the program.
//
// APC, PM, and DCS-shaped responses the emulator never asked for, plus mouse and focus reports. Each
// describes the program's or the window's own business rather than answering a question cm posed, so
// none may be matched against an outstanding request and none may be dropped.
//
// Zero when the sequence is incomplete or unrecognized, which makes SplitInput give up and forward the
// chunk whole.
func passthroughLen(p []byte) int {
	if len(p) < 2 || p[0] != 0x1b {
		return 0
	}
	switch p[1] {
	case '^':
		// PM. Nothing in cm asks a question answered by one, so it belongs to the program.
		return consumeString(p)
	case '[':
		// Mouse and focus reports. Every attached terminal is entitled to send its own, so these are
		// forwarded from each client rather than treated as a single session-wide answer.
		//
		// Scanned to the final byte the same way classifyReply does, rather than with the richer parser
		// in internal/query, so the two agree about extent within this package.
		i := 2
		for i < len(p) && !isCSIFinal(p[i]) {
			i++
		}
		if i >= len(p) {
			return 0
		}
		final, params, length := p[i], p[2:i], i+1
		if final == 'M' && len(params) == 0 {
			// X10 mouse carries three raw coordinate bytes after the final, which are consumed here so
			// one cannot be mistaken for a keystroke on the next pass.
			return min(length+3, len(p))
		}
		if len(params) > 0 && params[0] == '<' {
			return length
		}
		if len(params) == 0 && (final == 'I' || final == 'O') {
			return length
		}
		return 0
	}
	return 0
}

// classifyReply consumes one escape sequence, reporting its length and whether it answers a query.
//
// A length of zero means the sequence is incomplete, which is treated as not-a-reply by the caller:
// guessing on a fragment risks dropping the front of something that turns out to be typing.
func classifyReply(p []byte) (n int, reply bool) {
	if len(p) < 2 {
		return 0, false
	}

	switch p[1] {
	case '[':
		i := 2
		for i < len(p) && !isCSIFinal(p[i]) {
			i++
		}
		if i >= len(p) {
			return 0, false
		}
		final := p[i]
		params := p[2:i]
		length := i + 1

		// Mouse and focus are excluded deliberately, even though IsUserInput also calls them
		// not-typing. They report what happened to one window, so every attached terminal is entitled
		// to send its own.
		if final == 'M' && len(params) == 0 {
			return length, false
		}
		if len(params) > 0 && params[0] == '<' {
			return length, false
		}
		if len(params) == 0 && (final == 'I' || final == 'O') {
			return length, false
		}

		switch final {
		case 'R':
			// Cursor position report, answering CSI 6n.
			return length, true
		case 'n':
			// Device status report, answering CSI 5n. The request shares this final byte and is
			// distinguished only by its parameter: 5 and 6 are requests, and a reply carries the
			// status value instead. Matching the family would classify a client-sent CSI 6n as a reply
			// and drop it.
			if bytes.Equal(params, []byte("5")) || bytes.Equal(params, []byte("6")) {
				return length, false
			}
			return length, true
		case 'c':
			// Device attributes. A reply carries a private marker that a request does not, so a bare
			// CSI c arriving on the input channel is not treated as one.
			if len(params) > 0 && (params[0] == '?' || params[0] == '>' || params[0] == '=') {
				return length, true
			}
			return length, false
		case 'u':
			// Kitty keyboard flags, answering CSI ? u. A keypress in that protocol also ends in 'u',
			// so only the '?' form is a reply.
			if len(params) > 0 && params[0] == '?' {
				return length, true
			}
			return length, false
		case 'y':
			// DECRPM, answering DECRQM.
			return length, true
		case 't':
			// XTWINOPS size report.
			return length, true
		}
		return length, false

	case ']':
		// OSC. On this channel these are answers: a color report for OSC 10/11, or clipboard contents
		// for OSC 52.
		if end := consumeString(p); end > 0 {
			return end, true
		}
		return 0, false

	case 'P':
		// DCS, which carries XTGETTCAP and DECRQSS answers as well as XTVERSION.
		if end := consumeString(p); end > 0 {
			return end, true
		}
		return 0, false

	case '_':
		// APC. A kitty graphics response, which cm now proxies: internal/query classifies a=q as
		// terminal-only, so cm suppresses its model's answer, forwards the question, and relays the
		// terminal's reply in order.
		//
		// This deliberately changed. It used to be "not a reply", on the reasoning that cm asked no
		// graphics query so a response was the program's own business to receive. That was true at the
		// time and became false: cm's model was answering these all along, so a program got two answers
		// to one question, and the visible result was response text on the prompt line with typed
		// characters swallowed and a shell running "3" as a command.
		//
		// An unmatched one is still dropped by the proxy rather than forwarded, which is the behaviour
		// that broke `kitten icat` when it was first tried in isolation. It is safe now only because the
		// question is registered as outstanding before the terminal is asked, so a real answer matches.
		if end := consumeString(p); end > 0 {
			return end, true
		}
		return 0, false

	default:
		// PM, SS3, and alt-modified keys. Not recognized as replies.
		return 2, false
	}
}

// classifyEscape consumes one escape sequence, reporting its length and whether it is typing.
//
// A length of zero means the sequence is incomplete.
func classifyEscape(p []byte) (n int, typed bool) {
	if len(p) < 2 {
		return 0, false
	}

	switch p[1] {
	case '[':
		return classifyCSI(p)
	case ']':
		// OSC. On the input channel this is a reply, such as a clipboard read answering OSC 52, so
		// it is not typing.
		return consumeString(p), false
	case 'P', '_', '^':
		// DCS, APC, and PM. All replies on this channel, including the answer to a request for the
		// terminal's name.
		return consumeString(p), false
	case 'O':
		// SS3, which is how an application-mode cursor or keypad key arrives. Typed.
		if len(p) < 3 {
			return 0, false
		}
		return 3, true
	default:
		// ESC followed by one byte: alt-modified keys, and ESC itself when a user pressed it. Typed.
		return 2, true
	}
}

// classifyCSI consumes a CSI sequence and decides whether it is typing.
func classifyCSI(p []byte) (n int, typed bool) {
	// Find the final byte, which ends the sequence.
	i := 2
	for i < len(p) && !isCSIFinal(p[i]) {
		i++
	}
	if i >= len(p) {
		return 0, false
	}
	final := p[i]
	params := p[2:i]
	length := i + 1

	// Mouse reporting. X10 form is CSI M followed by three raw bytes, which are consumed here so a
	// coordinate byte cannot be mistaken for a keystroke on the next pass.
	if final == 'M' && len(params) == 0 {
		return min(length+3, len(p)), false
	}
	// SGR mouse: CSI < ... M or m.
	if len(params) > 0 && params[0] == '<' {
		return length, false
	}

	// Focus in and out, which a terminal sends when a window gains or loses focus. Emphatically not
	// typing: it is the event most likely to fire for a window the user is *not* using.
	if len(params) == 0 && (final == 'I' || final == 'O') {
		return length, false
	}

	switch final {
	case 'u':
		// Kitty keyboard flags, answering CSI ? u, which is a reply rather than a keystroke. The '?'
		// marker is the whole distinction: a keypress in this protocol carries a keycode where the
		// reply carries the marker, and both end in 'u'. classifyReply already splits them this way,
		// so leaving it out here made the two classifiers disagree about the same bytes.
		//
		// That disagreement is a bug rather than a cosmetic inconsistency, because service.go asks
		// IsUserInput *first* and treats anything it claims as typing. Reported against yazi, which
		// probes with DECRQSS, DECRQM, CSI ? u and DA1 in one burst; a real kitty answers all four in
		// a single write, measured as "\x1bP1$r1 q\x1b\\\x1b[?12;0$y\x1b[?0u". The trailing flags
		// reply made the whole blob read as typing, so instead of being matched against the questions
		// cm asked, it went to the pty verbatim. yazi received "r q" out of the DECRQSS reply, whose
		// "\x1bP1$" introducer it never saw: 'r' opens rename, then " q" is typed into the box. The
		// first run instead quit outright on the leaked 'q'. Injecting that exact blob into a live
		// yazi reproduces both.
		if len(params) > 0 && params[0] == '?' {
			return length, false
		}
		// The third parameter is the event type, where 3 means release. A release alone should not
		// claim sizing, since letting go of a key in a window you are leaving is not a reason to take
		// it over.
		if kittyEventType(params) == 3 {
			return length, false
		}
		return length, true
	case '~':
		// Function keys, and xterm's modifyOtherKeys form. Typed.
		return length, true
	case 'A', 'B', 'C', 'D', 'E', 'F', 'H', 'P', 'Q', 'S':
		// Arrows, home, end, and function keys. Typed.
		//
		// Note that these finals are also used by *output* sequences such as cursor movement, but
		// this function only ever sees the input channel, where they are keys.
		return length, true
	case 'R':
		// A cursor position report, which answers a query the program made. This is the case that
		// bit zmx: a program asking where the cursor is would otherwise hand sizing to whichever
		// window happened to answer.
		return length, false
	case 'c', 't', 'n':
		// Device attributes, window manipulation replies, and device status reports. All answers.
		return length, false
	default:
		// Unrecognized. Not typing, because a false positive costs a spurious reflow while a false
		// negative costs one extra keypress.
		return length, false
	}
}

// isCSIFinal reports whether a byte terminates a CSI sequence.
func isCSIFinal(b byte) bool {
	return b >= 0x40 && b <= 0x7e
}

// kittyEventType returns the event type from a kitty keyboard sequence's parameters, or 1 when
// unspecified, which means a press.
//
// The layout is `code:alternates ; modifiers:event ; text`, so the event type is the part after a
// colon in the second semicolon-separated field.
func kittyEventType(params []byte) int {
	field := 0
	for i := 0; i < len(params); i++ {
		if params[i] == ';' {
			field++
			if field > 1 {
				break
			}
			continue
		}
		if field != 1 || params[i] != ':' {
			continue
		}
		// Digits after the colon are the event type.
		n, ok := 0, false
		for j := i + 1; j < len(params) && params[j] >= '0' && params[j] <= '9'; j++ {
			n = n*10 + int(params[j]-'0')
			ok = true
		}
		if ok {
			return n
		}
	}
	return 1
}

// consumeString consumes a string-terminated sequence such as OSC or DCS.
//
// Both BEL and ST terminate one, and shells use both, so neither can be assumed.
func consumeString(p []byte) int {
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
