package input

// TerminalModes is what a program in the session has asked its terminal for.
//
// Passed in rather than read here because only the server holds the model. A struct rather than two
// bools so a third mode can be added without every caller changing shape.
type TerminalModes struct {
	// KittyKeyboard reports whether the kitty keyboard protocol is active, meaning some program pushed
	// flags and has not popped them.
	KittyKeyboard bool
	// FocusReports reports whether DECSET 1004 is enabled.
	FocusReports bool
}

// IsStaleEvent reports whether one sequence is a terminal event that no program in the session asked
// for, and which would therefore be typed at whatever is reading the pty now.
//
// The bug this exists for: quitting codex left "execute: 3u[O_" in zsh's line editor. codex pushes kitty
// keyboard flags 7, which includes report-event-types, and sets mode 1004. On ctrl-d it reads the key
// *press* and exits, and the *release* arrives after it is gone. cm forwarded it, zsh's line editor ate
// the ESC as a meta prefix and inserted the rest, so "\x1b[100;5:3u" showed as "3u" and a focus report
// "\x1b[O" as "[O". Racy in the obvious way: the program's own pop has to reach the terminal before the
// key is lifted, and under cm that is a round trip through the shim, the server and the client. It
// reproduced twice in three tries when codex was quit as soon as it opened.
//
// Measured, which is why this is a filter rather than a classifier fix: "\x1b[100;5:3u" and "\x1b[O" are
// both IsUserInput=false and IsQueryReply=false, and SplitInput returns nil for the release and a single
// part for the focus report. Service.Attach only splits a chunk when it yields more than one part, so
// both fell through to the verbatim pty write.
//
// **Deliberately narrow, and the narrowness is the design.** Only a key *release* and a focus report
// qualify, never a key press, because the two directions of being wrong are not comparable. Dropping a
// stale release loses nothing: no shell wants one, and a program that does want them degrades to not
// seeing releases. Dropping a press would make a session ignore the keyboard, and the model can read
// flags 0 while a program really has them on, most plausibly after a server restart rebuilds the model
// from a bounded log that no longer contains the push. So a press is always forwarded, and this stays a
// fix for the reported symptom rather than a general "cm knows better than the terminal" rule.
//
// Mouse reports are the same shape of bug and are deliberately not covered here: tracking left on by an
// exited program would leak reports the same way, but dropping one wrongly costs the user the mouse,
// which is worse than the artifact. See docs/ideas.md.
func IsStaleEvent(seq []byte, modes TerminalModes) bool {
	if len(seq) < 3 || seq[0] != 0x1b || seq[1] != '[' {
		return false
	}

	i := 2
	for i < len(seq) && !isCSIFinal(seq[i]) {
		i++
	}
	if i >= len(seq) {
		// Incomplete. Never stale, because guessing on a fragment is how a real keystroke gets dropped.
		return false
	}
	final, params := seq[i], seq[2:i]

	switch final {
	case 'u':
		if modes.KittyKeyboard {
			return false
		}
		// The flags reply carries the '?' marker and is a reply rather than an event, handled by the
		// query proxy. Excluded here so this never competes with it.
		if len(params) > 0 && params[0] == '?' {
			return false
		}
		return kittyEventType(params) == 3
	case 'I', 'O':
		// Focus in and out, which carry no parameters. A parameterized sequence ending in these finals
		// is something else and is left alone.
		return !modes.FocusReports && len(params) == 0
	}
	return false
}

// DropStaleEvents removes the stale events from a client's input chunk.
//
// Returns p unchanged, without allocating, when there is nothing to drop, which is every chunk a person
// types. Sequences it does not recognize are left in place, matching the conservative rule the rest of
// this package follows: a forwarded event is an artifact, a dropped keystroke is a session that ignores
// the keyboard.
func DropStaleEvents(p []byte, modes TerminalModes) []byte {
	if modes.KittyKeyboard && modes.FocusReports {
		// Nothing can be stale when a program wants both.
		return p
	}

	var out []byte
	start, i := 0, 0
	for i < len(p) {
		if p[i] != 0x1b {
			i++
			continue
		}
		n := csiLen(p[i:])
		if n <= 0 {
			// Not a CSI, or an incomplete one at the end of the chunk. Skip the ESC rather than the
			// whole tail, so a sequence following a lone ESC is still examined.
			i++
			continue
		}
		if IsStaleEvent(p[i:i+n], modes) {
			out = append(out, p[start:i]...)
			start = i + n
		}
		i += n
	}

	if out == nil && start == 0 {
		return p
	}
	return append(out, p[start:]...)
}

// DropStaleParts removes the stale events from already-framed input, dropping any part left empty.
//
// The form the server uses, since ReplyFramer.Split coalesces adjacent non-reply bytes into one part: a
// part can hold several sequences and a keystroke together, so each is filtered rather than kept or
// dropped whole.
//
// Reply parts are passed through untouched. A reply is matched against a question cm asked and is the
// query proxy's business; nothing here may compete with that.
func DropStaleParts(parts []Part, modes TerminalModes) []Part {
	if modes.KittyKeyboard && modes.FocusReports {
		return parts
	}

	out := parts[:0]
	for _, part := range parts {
		if part.Reply {
			out = append(out, part)
			continue
		}
		part.Data = DropStaleEvents(part.Data, modes)
		if len(part.Data) == 0 {
			continue
		}
		out = append(out, part)
	}
	return out
}

// csiLen reports the length of the CSI sequence at the start of p, or 0 if there is not a complete one.
func csiLen(p []byte) int {
	if len(p) < 3 || p[0] != 0x1b || p[1] != '[' {
		return 0
	}
	for i := 2; i < len(p); i++ {
		if isCSIFinal(p[i]) {
			return i + 1
		}
	}
	return 0
}
