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

// gateAction is what a read of keystrokes asks cm to do rather than the session.
type gateAction int

const (
	// gateNone means nothing cm intercepts was pressed.
	gateNone gateAction = iota
	// gateDetach means the detach key was pressed.
	gateDetach
	// gatePrefix means the prefix key was pressed, so the overlay opens.
	gatePrefix
)

// gateDecision is everything one read of keystrokes produced.
//
// A struct rather than several return values because the parts have to be read together: what the
// session receives, what cm does, and what is left over for whatever cm opened.
type gateDecision struct {
	// Forward is what the session should receive, which is whatever preceded the key.
	Forward []byte
	// Action is what cm must do itself.
	Action gateAction
	// Rest is what followed the prefix key in the same read.
	//
	// Non-empty when the prefix and the key after it land in one read, which happens when someone types
	// quickly or pastes. It belongs to the overlay rather than to the session. Detaching has no
	// equivalent: what follows a detach is dropped on purpose, since the user asked to leave.
	Rest []byte
}

// inputGate applies the keys cm intercepts to a client's keystrokes, deciding what the session sees.
//
// Its own type because the decision has several outcomes and one of them is time-dependent, which is
// most of what there is to get wrong here. Inline in the attach loop it was two conditions with no way
// to test either apart from a live attachment, and the missing bound above went unnoticed for that
// reason.
type inputGate struct {
	// detach ends the attachment, prefix opens the overlay. Both are matched the same way, and whichever
	// was pressed first in a read wins.
	detach KeySpec
	prefix KeySpec
	// suspended stops both keys being intercepted, so they reach the session like any other keystroke.
	//
	// Set while a nested client is attached inside this session, which the server reports. That client
	// reads its input from this session's pty, so the keys are only reachable by forwarding them, and the
	// inner gate is what recognizes them. Without this the outer client always won, which for a
	// per-window session meant ctrl-\ closed the window instead of leaving the inner session. The prefix
	// key follows the same rule for the same reason: the overlay belongs to the session the user is
	// looking at, which is the innermost one.
	//
	// Separate from KeySpec.Disabled, which is the configured "no key detaches". This one comes
	// and goes with the nesting and must not overwrite what the user configured.
	suspended bool
	// held is a partial encoding of the key, kept until the rest arrives or the grace expires.
	held []byte
	// heldAt is when the current held bytes were first withheld, so the deadline is measured from the
	// first byte rather than restarted by every later one.
	heldAt time.Time
}

// feed offers a read's worth of keystrokes to the gate.
func (g *inputGate) feed(data []byte, now time.Time) gateDecision {
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

	// Everything through, including anything withheld before the handover, and in the order it was
	// typed. Nothing is held back either: a partial sequence has no one here to complete it, and the
	// inner client needs the whole of it to recognize the key itself.
	if g.suspended {
		return gateDecision{Forward: buf}
	}

	// Whichever key was pressed first in this read wins, which is the only ordering that matches what the
	// user did. Detaching wins a tie, which is reachable only by configuring both keys to the same key:
	// leaving is the one that cannot be undone by pressing something else, so it is the safer reading.
	detachAt, _ := g.detach.find(buf)
	prefixAt, prefixLen := g.prefix.find(buf)
	switch {
	case detachAt >= 0 && (prefixAt < 0 || detachAt <= prefixAt):
		return gateDecision{Forward: buf[:detachAt], Action: gateDetach}
	case prefixAt >= 0:
		return gateDecision{
			Forward: buf[:prefixAt],
			Action:  gatePrefix,
			Rest:    buf[prefixAt+prefixLen:],
		}
	}

	// Hold back a possible partial sequence until the rest arrives, or until the grace expires. The
	// longer of the two, since a partial that could still become either key must wait for whichever needs
	// more bytes: with the defaults both encode as ESC [ 9 ... and diverge only at the fourth byte.
	if keep := max(g.detach.HoldBack(buf), g.prefix.HoldBack(buf)); keep > 0 && keep <= len(buf) {
		g.held = append(g.held, buf[len(buf)-keep:]...)
		if anchor.IsZero() {
			g.heldAt = now
		} else {
			g.heldAt = anchor
		}
		buf = buf[:len(buf)-keep]
	}
	return gateDecision{Forward: buf}
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
