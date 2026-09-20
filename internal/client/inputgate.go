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
	// gateNestedWarn means the detach key was pressed again while handed to an inner client that has not
	// acted on it, so the user is told that pressing it once more leaves this session instead.
	gateNestedWarn
)

// nestedPressesToWarn is how many forwarded detach keys it takes before cm says something, after which one
// more detaches this client whatever the handover says.
//
// Two, so the ordinary case is silent: one press is what leaves an inner session, and a notice on every
// nested detach would be noise on the common path. The press after the notice is the escape, which makes
// three in total -- enough that the keyboard's auto-repeat cannot reach it by accident, since repeat only
// begins after an initial delay of a quarter second or more and the notice sits between the second press
// and the third.
const nestedPressesToWarn = 2

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
	// nestedPresses counts detach keys forwarded since this handover began, which is what makes a handover
	// nobody is acting on escapable.
	//
	// The state it exists for: the inner client is gone but its parent does not know, so every press is
	// forwarded into nothing and the window cannot be left. That is reachable whichever way the nesting was
	// learned -- a dropped ssh withdraws no announcement, and an RPC-known client can wedge -- so the way
	// out is the same for both rather than a rule per case.
	//
	// Counted rather than timed, and that is the part worth keeping. A press within a window of the last one
	// would be the obvious spelling and is unsafe: holding ctrl-\ repeats at about 30/s once the keyboard's
	// initial delay expires, so a stuck key would detach the inner session and then close the window, which
	// is the failure this whole mechanism exists to prevent. A count that resets whenever the nesting
	// changes cannot do that, because a press the inner client acts on changes the nesting and clears it.
	nestedPresses int
	// nestedCount is how many clients the server last said were nested, which is what "the nesting changed"
	// is measured against.
	//
	// The boolean is not enough, and this was measured rather than reasoned: with four levels nested, the
	// aggregate stays nested while each press leaves one of them, so every working press looked unanswered
	// and the third one detached the outer window with a live client still inside it. That is the failure the
	// handover exists to prevent, reintroduced by its own escape.
	nestedCount int
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
		// Scanned even though nothing is intercepted, because a handover that is not being acted on has to
		// be escapable: see nestedPresses. Counted once per read rather than per occurrence, which is the
		// conservative direction -- a burst in one read is one press, so a paste cannot reach the escape.
		if at, _ := g.detach.find(buf); at >= 0 {
			g.nestedPresses++
			switch {
			case g.nestedPresses == nestedPressesToWarn:
				// Forwarded as well as reported. The inner client may be alive and merely slow, in which case
				// this press is its own and the notice is the only thing added.
				return gateDecision{Forward: buf, Action: gateNestedWarn}
			case g.nestedPresses > nestedPressesToWarn:
				// Acted on here, so the key is taken out of what goes on rather than sent to a client that
				// has had two of them and done nothing.
				return gateDecision{Forward: buf[:at], Action: gateDetach}
			}
		}
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

// setNesting records what is nested inside this session, which is what hands the keys over.
//
// A method rather than an assignment so the press count cannot be left behind. Reset on every change,
// including one level of several going: a press the inner client acted on changes the count, and a count
// that survived that would bring the escape within reach while live clients remain.
func (g *inputGate) setNesting(nested bool, count int) {
	if g.suspended == nested && g.nestedCount == count {
		return
	}
	g.suspended, g.nestedCount = nested, count
	g.nestedPresses = 0
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
