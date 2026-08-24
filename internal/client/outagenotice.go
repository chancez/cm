package client

import (
	"fmt"
	"io"
	"strings"
	"time"
)

// outageNotice paints one line at the bottom of the terminal while the server is missing.
//
// Why anything is painted at all: the client holds the terminal through an outage, so a wait is
// indistinguishable from a hang. The session is fine and the shell is still running, but nothing says so,
// and the recovery is invisible too. A server stopped by accident left every window frozen with no
// explanation, and the way out was to open a new window and run any cm command, which starts one.
//
// Deliberately not painted for a routine restart. reconnectQuietPeriod is the same threshold the log uses
// and for the same reason: a restart takes about 450ms, so anything faster is the expected case and a
// flash of text on every upgrade would be worse than silence.
//
// The line overwrites whatever the session had on its bottom row, which is why recovery repaints from a
// fresh attach rather than resuming. cm's terminal model holds the real content and the client does not,
// so repainting is the only way to put that row back. See Attach.
type outageNotice struct {
	// out is where the line is painted, which is the terminal.
	out io.Writer
	// size reports the terminal's current size. Zeros mean it could not be determined, and nothing is
	// painted then: a row number guessed wrong would write into the middle of the session.
	size func() (rows, cols uint16)
	// enabled is false for anything that is not painting a terminal. A follower streaming to a pipe must
	// never receive escape bytes, which is the same distinction the gap repaint draws on NoRestore.
	enabled bool
	// quietFor is how long an outage has to last before anything is painted. Zero paints immediately.
	//
	// A field rather than the constant read directly, so a test can exercise the painting and the recovery
	// it forces without spending the threshold on every run. Attach sets it to reconnectQuietPeriod.
	quietFor time.Duration

	// painted records whether the line is currently on screen, so recovery knows whether it has a row to
	// put back, and so an unchanged second is not rewritten.
	painted bool
	// last is the text on screen, compared before repainting. Without it every retry would rewrite an
	// identical line, and a terminal being written to is a terminal that cannot be idle.
	last string
}

// update paints the notice for an outage that has lasted this long, or does nothing while it is routine.
//
// waited is passed in rather than measured here so the decision is testable without sleeping, which is
// the same reason report takes the outage rather than a clock.
func (n *outageNotice) update(waited time.Duration, note string) {
	if !n.enabled || waited < n.quietFor {
		return
	}
	rows, cols := n.size()
	if rows == 0 || cols == 0 {
		return
	}

	text := noticeText(waited, note, int(cols))
	if n.painted && text == n.last {
		return
	}
	// DECSC and DECRC around the write, so the cursor the session is using is exactly where it was.
	// Column 1 of the last row, cleared first, so a shorter notice cannot leave the tail of a longer one
	// behind.
	fmt.Fprintf(n.out, "\x1b7\x1b[%d;1H\x1b[2K\x1b[7m%s\x1b[0m\x1b8", rows, text)
	n.painted = true
	n.last = text
}

// clear erases the notice, reporting whether there was one to erase.
//
// The caller needs that answer: a row this overwrote has to be repainted from the session's own content,
// which the client does not have.
func (n *outageNotice) clear() bool {
	if !n.painted {
		return false
	}
	rows, _ := n.size()
	if rows > 0 {
		fmt.Fprintf(n.out, "\x1b7\x1b[%d;1H\x1b[2K\x1b8", rows)
	}
	n.painted = false
	n.last = ""
	return true
}

// noticeText builds the line, fitted to the terminal's width.
//
// Truncated to one column short of the width on purpose. Writing the final column leaves the terminal in
// its pending-wrap state, and a single further byte would scroll the screen: that would move the
// session's content up by a row and desynchronize it from the model that is about to repaint it. One
// unused column costs nothing by comparison.
func noticeText(waited time.Duration, note string, cols int) string {
	text := fmt.Sprintf(" cm: lost the server, reconnecting (%s) ", waited.Round(time.Second))
	if note != "" {
		// The elapsed time stays, whatever the note says: a window that has been waiting two minutes reads
		// very differently from one that has been waiting five seconds, and the note alone does not say.
		text = fmt.Sprintf(" cm: %s (%s) ", note, waited.Round(time.Second))
	}
	// Collapsed to one line, since a reason read from the server's stderr can carry newlines and a
	// newline here would scroll the screen.
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if limit := cols - 1; limit > 0 && len(text) > limit {
		text = text[:limit]
	}
	return text
}
