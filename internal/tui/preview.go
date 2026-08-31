package tui

import (
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/chancez/cm/internal/ansi"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// preview is the pane under the list showing the selected session's last output.
//
// What it is for is recognising a session. Half of them are named by the server, so the name says
// nothing, and a directory shared by four sessions says little more; the last few lines of what a
// session printed are what a person actually recognises it by.
type preview struct {
	// on reports whether the pane is shown. Toggled by the user, since it costs a request per refresh
	// and a third of the screen.
	on bool

	// want is the session the pane should be showing, which is whatever is selected now.
	//
	// Separate from have, and that separation is the point. A request is in flight for a session that
	// may no longer be selected by the time it answers, and content painted under the wrong row is
	// worse than an empty pane: it is a confident answer to a question nobody asked. See previewMsg.
	want string
	// have is the session the lines below actually came from, empty when there are none.
	have string

	lines []string
	err   error

	// due marks the content as worth re-reading even though the selection has not changed, which is
	// what a refresh means: the session is still running and its last lines move.
	due bool

	// height is the whole pane including its header, and width its content. Zero height means the pane
	// is not drawn, either because it is off or because the window is too short to give it room.
	height int
	width  int
}

// previewMsg is a completed read of one session's output.
type previewMsg struct {
	// sessionID is which session this is, so a late answer can be discarded rather than painted under
	// whatever is selected by the time it arrives.
	sessionID string
	data      []byte
	err       error
}

// Pane sizing.
//
// The pane sits under the list rather than beside it, which is a decision about what makes a session
// recognisable: its output is lines of text written for a terminal's full width, and a narrow column
// beside the list would truncate every one of them. The list's own rows are wide too, since they carry
// a name, a state and a path.
const (
	// previewMinRows is the smallest pane worth drawing: a header and five lines. Fewer lines than that
	// shows the shell prompt and nothing above it, which is the one thing every session has in common.
	previewMinRows = 6
	// listMinRows is what the list keeps for itself. Below this the picker stops being a list, and a
	// window this short is one where the pane is the thing to give up.
	listMinRows = 5
)

// previewHeightFor divides the space between the list and the pane.
//
// Two fifths to the pane, so the list stays the larger half: the pane identifies the row the cursor is
// on, and the list is what the cursor moves through. Zero when the window cannot spare the room, which
// the caller renders as no pane at all rather than as a squeezed one.
func previewHeightFor(avail int) int {
	if avail-previewMinRows < listMinRows {
		return 0
	}
	height := avail * 2 / 5
	if height < previewMinRows {
		height = previewMinRows
	}
	if avail-height < listMinRows {
		height = avail - listMinRows
	}
	return height
}

// fetchPreview reads the tail of one session's output.
//
// ref is the ID reference rather than the name, for the same reason attaching uses one: a name is a
// binding and can be pointed at another session between the refresh that drew the row and this
// request. Reading the wrong session is quieter than attaching to it and no less wrong.
//
// lines is the pane's content height, so the server renders and returns about what will be shown rather
// than a screen's worth to be thrown away.
func (m model) fetchPreview(id, ref string, lines int) tea.Cmd {
	return func() tea.Msg {
		resp, err := m.sessions.Read(m.ctx, &serverv1.ReadRequest{
			Session: ref,
			Lines:   uint32(lines),
			// Deliberately not unwrapped. Unwrapping rejoins soft-wrapped lines into one long line, which
			// is right for a caller parsing output and wrong here: the pane truncates at its width, so a
			// rejoined line would hide everything the terminal had already wrapped onto the next row.
			Unwrap: false,
		})
		if err != nil {
			return previewMsg{sessionID: id, err: err}
		}
		return previewMsg{sessionID: id, data: resp.Data}
	}
}

// previewLines turns a session's output into the pane's rows.
//
// Session content is not trusted, and that is the whole reason this function exists. The bytes come
// from whatever program is running, they are about to be placed inside a frame bubbletea composes, and
// a single escape byte among them corrupts that frame: cm's largest family of bugs is exactly this,
// something writing to a terminal without knowing what state the stream is in. The plain form of Read
// should contain no escapes at all, since it renders cells rather than replaying bytes, so this is
// belt and braces on purpose. "Should" is what the other five instances of this bug also had.
func previewLines(data []byte, width, height int) []string {
	if width <= 0 || height <= 0 {
		return nil
	}

	// Escape sequences, CR, BS and BEL go first, which internal/ansi already does and is tested for.
	text := string(ansi.Strip(data))

	var b strings.Builder
	b.Grow(len(text))
	col := 0
	for _, r := range text {
		switch {
		case r == '\n':
			b.WriteRune(r)
			col = 0
		case r == '\t':
			// Expanded rather than passed through. A tab moves the cursor to the next stop, which the
			// width accounting in this package cannot see, so a tab inside a frame shifts everything after
			// it and the pane stops lining up with the list above it. internal/ansi keeps tabs
			// deliberately, because a build log's columns matter to whoever reads it; here the frame's
			// columns matter more.
			n := 8 - col%8
			b.WriteString(strings.Repeat(" ", n))
			col += n
		case r < 0x20 || r == 0x7f:
			// Anything else in the C0 range is dropped. internal/ansi removes the three that appear in
			// practice; this covers the rest, since a form feed or a NUL in a frame is as bad as an escape
			// and neither carries content a person wants to read.
		default:
			b.WriteRune(r)
			col++
		}
	}

	lines := strings.Split(strings.TrimRight(b.String(), "\n"), "\n")
	// Trailing blank lines are dropped before the tail is taken, so the pane fills with content rather
	// than with the empty rows a terminal leaves below a prompt.
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) > height {
		lines = lines[len(lines)-height:]
	}
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		// Truncated rather than wrapped. A wrapped line would take rows the pane counted on for other
		// lines, so the oldest content would scroll out of a pane that looks like it fits.
		out = append(out, truncate(line, width))
	}
	return out
}

// view renders the pane, header included, at exactly its height.
//
// Padded to that height rather than left short, so the footer under it does not move as content arrives
// or a session goes quiet. A pane that changes height on every refresh reads as flicker.
func (p preview) view(label string) string {
	if p.height <= 0 {
		return ""
	}
	rows := make([]string, 0, p.height)
	rows = append(rows, p.header(label))

	body := p.lines
	switch {
	case p.err != nil:
		// The reason rather than a blank pane. A session whose output cannot be read is usually one
		// running on a build without the terminal model, and an empty pane invites the reader to think
		// the session is idle.
		body = []string{"cannot read this session: " + p.err.Error()}
	case p.have == "":
		body = []string{"reading..."}
	case len(body) == 0:
		body = []string{"nothing has been printed yet"}
	}
	for _, line := range body {
		if len(rows) == p.height {
			break
		}
		rows = append(rows, line)
	}
	for len(rows) < p.height {
		rows = append(rows, "")
	}
	return strings.Join(rows, "\n")
}

// header names the session the pane is showing.
//
// Worth a row of its own: the pane is content from somewhere else placed under a list, and without a
// label the reader has to trust that it belongs to the highlighted row. The bug this guards against is
// real enough that the code has a discard for it, and a label makes a mistake visible rather than
// plausible.
func (p preview) header(label string) string {
	if label == "" {
		label = "no session"
	}
	head := "-- " + label + " "
	if width := lipgloss.Width(head); p.width > width {
		head += strings.Repeat("-", p.width-width)
	}
	return faintStyle.Render(truncate(head, p.width))
}
