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
		// Kitty keyboard protocol. The third parameter is the event type, where 3 means release.
		// A release alone should not claim sizing, since letting go of a key in a window you are
		// leaving is not a reason to take it over.
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
