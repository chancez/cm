package client

import (
	"fmt"
	"io"
)

// nestedNotice paints one line saying the detach key is being handed to an inner client that is not acting
// on it, and that pressing it again leaves this session instead.
//
// Why it exists: while something is nested inside this session, both keys cm intercepts are forwarded, so a
// nested client that has gone without saying so leaves the window with nothing that answers. That state is
// reachable from either direction -- a dropped ssh withdraws no announcement, and a client the server knows
// about can wedge -- and the escape is the detach key itself, which needs saying out loud or nobody will
// find it.
//
// The notice is the safety rather than decoration. Without it two presses would be the escape, and holding
// ctrl-\ repeats fast enough that a stuck key would detach the inner session and then close the window.
// With it, the third press is a deliberate answer to something on screen.
//
// Same row and same sequence as the outage notice, through paintBottomRow, and the same consequence: it
// overwrites whatever the session had there, so clearing it owes a repaint from cm's model. See Attach.
type nestedNotice struct {
	// out is where the line is painted, which is the terminal.
	out io.Writer
	// size reports the terminal's current size. Zeros mean it could not be determined, and nothing is
	// painted then: a row number guessed wrong would write into the middle of the session.
	size func() (rows, cols uint16)
	// enabled is false for anything not painting a terminal, the same distinction the outage notice draws.
	enabled bool

	// painted records whether the line is on screen, so a caller knows whether it owes a repaint.
	painted bool
}

// show paints the notice, naming the key so the escape is discoverable rather than folklore.
func (n *nestedNotice) show(detach KeySpec) {
	if !n.enabled || n.painted {
		return
	}
	rows, cols := n.size()
	if rows == 0 || cols == 0 {
		return
	}
	key := detach.Name
	if key == "" {
		key = DefaultDetachKey
	}
	text := fitNoticeLine(
		fmt.Sprintf(" cm: %s went to the session nested here, which has not answered; press it again to "+
			"detach this one ", key), int(cols))
	paintBottomRow(n.out, rows, text)
	n.painted = true
}

// clear erases the notice, reporting whether there was one to erase.
//
// The answer is the caller's business: the row this overwrote held the session's content, and cm's model on
// the server is the only thing that knows what was there.
func (n *nestedNotice) clear() bool {
	if !n.painted {
		return false
	}
	rows, _ := n.size()
	if rows > 0 {
		eraseBottomRow(n.out, rows)
	}
	n.painted = false
	return true
}
