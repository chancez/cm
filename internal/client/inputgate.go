package client

import "time"

// escapeGrace bounds how long a partial detach-key sequence is withheld while the rest of it is
// awaited.
//
// The holdback it bounds is not optional. A terminal with the kitty keyboard protocol or xterm's
// modifyOtherKeys reports ctrl-\ as a CSI sequence rather than as 0x1C, and such a sequence can be
// split across two reads, so a client that matched only within a single read would forward half of it
// to the shell and act on nothing. zmx hit that with Claude Code, which enables modifyOtherKeys on
// startup, and the detach key stopped working entirely.
//
// What was missing is a bound. Every encoding starts with ESC, so a lone escape looked like a partial
// sequence and was withheld until the next read, which may never come: pressing escape and then
// waiting delivered nothing at all. That is the keypress that leaves insert mode in zsh's vi mode, in
// vim, and in Claude Code, so the visible symptom was a mode that did not change until another key
// was pressed, and that key then being interpreted in the mode the user thought they had left.
//
// 50ms, which is two orders of magnitude above the gap this has to cover and below the point where a
// keypress feels late. A terminal writes a whole key sequence in one write, so the halves of a split
// arrive microseconds apart on the same host; the case that needs any window at all is a sequence
// divided between two network frames. The same reasoning produced vim's ttimeoutlen default of 50ms
// and neovim's. tmux's escape-time defaults to 500ms and is the setting everyone turns down, which is
// the error worth not repeating: it is chosen for a link far worse than this has to survive, and it
// makes escape feel broken.
//
// The cost is stated rather than hidden: a detach sequence whose halves arrive more than this far
// apart is no longer recognized, so its first byte reaches the program and the remainder is typed at
// it. See "Recognizing the detach key" in docs/architecture.md for the alternatives and why this
// one was preferred.
const escapeGrace = 50 * time.Millisecond

// inputGate applies the detach key to a client's keystrokes, deciding what the session should see.
//
// Its own type because the decision has three outcomes and one of them is time-dependent, which is
// most of what there is to get wrong here. Inline in the attach loop it was two conditions with no way
// to test either apart from a live attachment, and the missing bound above went unnoticed for that
// reason.
type inputGate struct {
	key DetachKeySpec
	// held is a partial encoding of the key, kept until the rest arrives or the grace expires.
	held []byte
	// heldAt is when the current held bytes were first withheld, so the deadline is measured from the
	// first byte rather than restarted by every later one.
	heldAt time.Time
}

// feed offers a read's worth of keystrokes to the gate.
//
// forward is what the session should receive, which is empty when everything was consumed. detach
// reports that the key was pressed, in which case forward holds whatever preceded it and anything
// after it is deliberately dropped: the user asked to leave, so later keystrokes belong to whatever
// comes next.
func (g *inputGate) feed(data []byte, now time.Time) (forward []byte, detach bool) {
	// The existing anchor is kept across the rejoin below, so a partial that grows over several reads
	// is still released a fixed time after its *first* byte. Restarting the clock on each read would
	// let a stream that keeps ending in a partial postpone the release indefinitely, which is the
	// unbounded wait this whole mechanism exists to remove.
	anchor := g.heldAt

	buf := data
	if len(g.held) > 0 {
		buf = append(g.held, data...)
		g.held = nil
	}
	g.heldAt = time.Time{}

	if i := g.key.Find(buf); i >= 0 {
		return buf[:i], true
	}

	// Hold back a possible partial sequence until the rest arrives, or until the grace expires.
	if keep := g.key.HoldBack(buf); keep > 0 && keep <= len(buf) {
		g.held = append(g.held, buf[len(buf)-keep:]...)
		if anchor.IsZero() {
			g.heldAt = now
		} else {
			g.heldAt = anchor
		}
		buf = buf[:len(buf)-keep]
	}
	return buf, false
}

// deadline reports when the held bytes must be released, and whether anything is held at all.
func (g *inputGate) deadline() (time.Time, bool) {
	if len(g.held) == 0 {
		return time.Time{}, false
	}
	return g.heldAt.Add(escapeGrace), true
}

// flush releases the held bytes to the session, which is what makes a lone escape arrive.
func (g *inputGate) flush() []byte {
	if len(g.held) == 0 {
		return nil
	}
	out := g.held
	g.held = nil
	g.heldAt = time.Time{}
	return out
}
