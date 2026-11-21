package client

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// shelvedChoice is how a client left the shelved notice.
type shelvedChoice int

const (
	// shelvedRestore is the Restore button: open the session again, which revives it.
	shelvedRestore shelvedChoice = iota + 1
	// shelvedDetach is the Detach button, which leaves the way the detach key does.
	shelvedDetach
	// shelvedRunning means the session is running again, restored from somewhere else, so this client goes
	// back to it as though it had pressed Restore.
	shelvedRunning
	// shelvedGone means the session no longer exists, forgotten or killed while this client waited.
	shelvedGone
)

// shelvedPoll is how often a client on the shelved notice asks whether the session is running again.
//
// What makes every window on a shelved session come back when any one of them, or `cm restore`, restores it.
// One List per half second per waiting client, against a server that answers it from memory and the store.
const shelvedPoll = 500 * time.Millisecond

// Mouse reporting while the notice is up: button presses, in SGR encoding, so a click on a button arrives
// as `ESC [ < b ; x ; y M` with no limit on the column. Turned on only here: the session's own program
// decides about the mouse while it is attached, and its modes are restored with its screen.
const (
	shelvedMouseOn  = "\x1b[?1000h\x1b[?1006h"
	shelvedMouseOff = "\x1b[?1000l\x1b[?1006l"
)

// shelvedNotice is what a client shows once `cm shelve` has ended the session it is attached to: the
// session's last screen, dimmed, under a box saying it is shelved, with a Restore and a Detach button.
//
// Shelving from somewhere else, `cm tui`, the CLI or kspaces' keeper, used to close every window attached to
// the session, since the stream ended as it does for a shell that exits. The client stays up instead, the
// way it does through a server restart, and the window keeps its place.
//
// It owns the whole screen, unlike the overlay, which paints a block of rows over a session that is still
// drawing: nothing is attached, so nothing else writes, and the screen under it is a snapshot from the
// persisted log rather than a live one.
type shelvedNotice struct {
	// session is what the box calls the session.
	session string
	// readOnly drops the Restore button, since a read-only client does not start processes: a read-only
	// attach to a shelved session is refused for the same reason. It still goes back when the session runs.
	readOnly bool
	// lines is the session's last screen as text, top line first.
	lines []string
	// focus is the button enter presses, an index into buttons.
	focus int

	barStyle, bodyStyle, selectedStyle Style

	// hits are where the buttons were last painted, for a click to be matched against.
	hits []shelvedHit
}

// shelvedHit is the cells one button covers, 1-based like the mouse reports.
type shelvedHit struct {
	row, from, to int
	choice        shelvedChoice
}

// shelvedButton is a button's label and what pressing it chooses.
type shelvedButton struct {
	label  string
	choice shelvedChoice
}

func (n *shelvedNotice) buttons() []shelvedButton {
	detach := shelvedButton{label: "[ Detach  d ]", choice: shelvedDetach}
	if n.readOnly {
		return []shelvedButton{detach}
	}
	return []shelvedButton{{label: "[ Restore  r ]", choice: shelvedRestore}, detach}
}

// paint returns the bytes that draw the notice on a terminal of this size.
//
// The screen is cleared and redrawn whole, every time: there is no program drawing underneath, so a full
// redraw on a resize is the simplest correct thing. The last screen is written faint and uncoloured, which
// is the dimming, and every row is cut one column short of the width, for the pending-wrap reason the
// overlay gives.
func (n *shelvedNotice) paint(rows, cols int) string {
	var b strings.Builder
	b.WriteString("\x1b[?25l" + shelvedMouseOn + "\x1b[0m\x1b[H\x1b[2J")
	if rows <= 0 || cols <= 1 {
		return b.String()
	}
	width := cols - 1
	for i, line := range n.lines {
		if i >= rows {
			break
		}
		fmt.Fprintf(&b, "\x1b[%d;1H\x1b[2m%s\x1b[0m", i+1, clip(line, width))
	}

	title := n.session + " is shelved"
	note := "Its processes have ended. Restore starts them again."
	if n.readOnly {
		note = "Its processes have ended."
	}
	buttons := n.buttons()
	labels := make([]string, len(buttons))
	for i, btn := range buttons {
		labels[i] = btn.label
	}
	buttonRow := strings.Join(labels, "  ")
	inner := max(len([]rune(title)), len(note), len(buttonRow))
	boxWidth := min(inner+4, width)
	left := max((cols-boxWidth)/2, 0) + 1
	// Blank, title, note, blank, buttons, blank.
	const boxRows = 6
	top := max((rows-boxRows)/2, 0) + 1
	text := func(row int, style Style, s string) {
		if row > rows {
			return
		}
		fmt.Fprintf(&b, "\x1b[%d;%dH%s%s%s", row, left, style, pad(clip("  "+s, boxWidth), boxWidth), overlayStyleOff)
	}
	text(top, n.bodyStyle, "")
	text(top+1, n.barStyle, title)
	text(top+2, n.bodyStyle, note)
	text(top+3, n.bodyStyle, "")
	text(top+4, n.bodyStyle, "")
	text(top+5, n.bodyStyle, "")

	// The buttons over the blank row painted for them, centred in the box, each in its own style: the
	// focused one in the bar's, which is the strongest contrast, and the other as a selected row.
	n.hits = n.hits[:0]
	if row := top + 4; row <= rows {
		col := left + max((boxWidth-len(buttonRow))/2, 0)
		for i, btn := range buttons {
			style := n.selectedStyle
			if i == n.focus {
				style = n.barStyle
			}
			if end := col + len(btn.label) - 1; end < left+boxWidth {
				fmt.Fprintf(&b, "\x1b[%d;%dH%s%s%s", row, col, style, btn.label, overlayStyleOff)
				n.hits = append(n.hits, shelvedHit{row: row, from: col, to: end, choice: btn.choice})
			}
			col += len(btn.label) + 2
		}
	}
	return b.String()
}

// feed reads keystrokes and clicks, and reports what was chosen.
//
// r restores and d detaches, as the buttons say, and so does the detach key, q, and ctrl-c, which here has
// nothing to interrupt. Tab and the arrows move the focus and enter presses the focused button. A click
// presses the button under it. Anything else is ignored: nothing is attached for it to go to.
func (n *shelvedNotice) feed(p []byte, detach keySet) (shelvedChoice, bool) {
	if i, _, _ := detach.find(p); i >= 0 {
		return shelvedDetach, true
	}
	buttons := n.buttons()
	for i := 0; i < len(p); i++ {
		switch c := p[i]; c {
		case 'r', 'R':
			if !n.readOnly {
				return shelvedRestore, true
			}
		case 'd', 'D', 'q', 'Q', 0x03:
			return shelvedDetach, true
		case '\t', 'h', 'l':
			n.focus = (n.focus + 1) % len(buttons)
		case '\r', '\n', ' ':
			return buttons[min(n.focus, len(buttons)-1)].choice, true
		case 0x1b:
			end, choice, ok := n.escape(p[i:])
			if ok {
				return choice, true
			}
			i += end - 1
		}
	}
	return 0, false
}

// escape reads one escape sequence at the start of p: a click, or an arrow that moves the focus. It returns
// how many bytes the sequence took, and a choice when it was a click on a button.
func (n *shelvedNotice) escape(p []byte) (int, shelvedChoice, bool) {
	if len(p) < 3 || p[1] != '[' {
		return 1, 0, false
	}
	// A CSI runs to its final byte, 0x40 to 0x7e.
	end := 2
	for end < len(p) && (p[end] < 0x40 || p[end] > 0x7e) {
		end++
	}
	if end == len(p) {
		return len(p), 0, false
	}
	final := p[end]
	end++
	switch {
	case p[2] == '<' && final == 'M':
		if choice, ok := n.click(p[3 : end-1]); ok {
			return end, choice, true
		}
	case final == 'C' || final == 'D' || final == 'Z':
		n.focus = (n.focus + 1) % len(n.buttons())
	}
	return end, 0, false
}

// click reads an SGR mouse press, `b;x;y`, and reports the button a left click landed on.
func (n *shelvedNotice) click(params []byte) (shelvedChoice, bool) {
	fields := bytes.Split(params, []byte(";"))
	if len(fields) != 3 {
		return 0, false
	}
	nums := make([]int, 3)
	for i, f := range fields {
		v, err := strconv.Atoi(string(f))
		if err != nil {
			return 0, false
		}
		nums[i] = v
	}
	// The left button, without modifiers or motion.
	if nums[0] != 0 {
		return 0, false
	}
	for _, h := range n.hits {
		if nums[2] == h.row && nums[1] >= h.from && nums[1] <= h.to {
			return h.choice, true
		}
	}
	return 0, false
}

// shelvedLeave is what the notice writes on its way off screen, whichever way it goes: mouse reporting
// off, and the screen cleared with the cursor back. A restore repaints the session over it; a detach hands
// the terminal back through TTY.Close, which resets it anyway.
const shelvedLeave = shelvedMouseOff + "\x1b[0m\x1b[H\x1b[2J\x1b[?25h"

// waitShelved shows the shelved notice until the user restores or detaches, or the session runs again or
// goes away.
//
// Its own connection, separate from the attach stream the server has closed, used to read the last screen
// and to ask whether the session is running. A server that cannot be reached leaves the notice as it is and
// is tried again at the next poll, as an outage is.
func waitShelved(
	ctx context.Context, tty *TTY, opts *Options, n *shelvedNotice, id string,
	in *terminalInput, winch <-chan os.Signal, detach keySet,
) (shelvedChoice, error) {
	out := injectWriter{opts.screen}
	ref := paths.FormatSessionID(id)
	defer fmt.Fprint(out, shelvedLeave)

	paint := func() {
		rows, cols := tty.Size()
		fmt.Fprint(out, n.paint(int(rows), int(cols)))
	}
	// The last screen, fetched again on a resize so it fits the new width.
	fetch := func() {
		cl, closeConn, err := connectBrief(ctx, opts)
		if err != nil {
			return
		}
		defer closeConn()
		rows, _ := tty.Size()
		resp, err := cl.Read(ctx, &serverv1.ReadRequest{Session: ref, Lines: uint32(rows)})
		if err != nil {
			opts.Log.Debug("reading a shelved session's last screen failed", "session", ref, "error", err)
			return
		}
		n.lines = strings.Split(strings.TrimRight(string(resp.Data), "\n"), "\n")
	}
	fetch()
	paint()

	tick := time.NewTicker(shelvedPoll)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-winch:
			fetch()
			paint()
		case data, ok := <-in.data:
			if !ok {
				// The window is gone, as for an attached client.
				return shelvedDetach, nil
			}
			if choice, done := n.feed(data, detach); done {
				return choice, nil
			}
			paint()
		case <-tick.C:
			state, found, err := sessionState(ctx, opts, id)
			switch {
			case err != nil:
				opts.Log.Debug("asking whether a shelved session is running failed", "session", ref, "error", err)
			case !found:
				return shelvedGone, nil
			case state == serverv1.SessionState_SESSION_STATE_RUNNING:
				return shelvedRunning, nil
			}
		}
	}
}

// connectBrief opens a connection for one or two calls, with a function that closes it.
func connectBrief(ctx context.Context, opts *Options) (serverv1.ServerClient, func(), error) {
	conn, cl, err := opts.connect(ctx)
	if err != nil {
		return nil, nil, err
	}
	return cl, func() { conn.Close() }, nil
}

// sessionState asks the server for a session's state by ID, reporting whether it still exists.
func sessionState(ctx context.Context, opts *Options, id string) (serverv1.SessionState, bool, error) {
	cl, closeConn, err := connectBrief(ctx, opts)
	if err != nil {
		return 0, false, err
	}
	defer closeConn()
	resp, err := cl.List(ctx, &serverv1.ListRequest{})
	if err != nil {
		return 0, false, err
	}
	for _, s := range resp.Sessions {
		if s.GetId() == id {
			return s.GetState(), true, nil
		}
	}
	return 0, false, nil
}
