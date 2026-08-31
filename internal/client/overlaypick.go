package client

import (
	"fmt"
	"os"
	"strings"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// picker is a one-column chooser drawn in the overlay's own rows.
//
// It exists because typing was the worst part of the first version of this overlay: naming a session you
// are looking at is unavoidable typing, but *choosing* one that already exists is not, and asking someone
// to type a session name they can see in `cm list` is the kind of friction that stops a feature being
// used.
//
// Deliberately not bubbletea's list, which cm already has in internal/tui and which is better in every way
// except the one that matters here: it owns the whole terminal. This has to fit under a program that is
// still drawing, so it is a handful of rows, one column, no preview. Anything richer is what the overlay's
// `t` binding is for, which hands the terminal to the real picker.
//
// Filter-first, like fzf: every printable key narrows the list, and moving is the arrows or ctrl-n and
// ctrl-p. The alternative, j and k for movement, cannot coexist with a filter, and a filter is what makes
// twenty sessions usable at all.
type picker struct {
	// prompt names what the choice is for, shown on the bar.
	prompt string
	// action is what the caller does with the chosen item.
	action pickAction
	// items are everything choosable, unfiltered.
	items []pickItem
	// filter narrows items by substring, case-insensitively.
	filter []rune
	// cursor indexes the *filtered* list.
	cursor int
	// height is how many rows the list occupies, fixed once the list has arrived.
	//
	// Fixed rather than following the number of matches, because the block is anchored to the bottom of the
	// screen: a list that shrinks as you type moves every row under your eyes and repaints the program's
	// content between keystrokes. It grows exactly once, when the list replaces "listing sessions...", and
	// then holds still. Clamped to the space available on every render, so a resize still shrinks it.
	height int
	// loading is true until the session list arrives, since it comes from the server.
	loading bool
	// err is why the list could not be fetched.
	err string
}

// pickItem is one choosable thing.
type pickItem struct {
	// Ref is what a command acts on, and it is the session's ID rather than a name.
	//
	// A name can be moved to another session between this list arriving and a choice being made, which
	// would send the window somewhere the user did not pick. An ID cannot: see AGENTS.md on the two
	// spellings.
	Ref string
	// Label is what the user recognizes: the session's name, or its ID when it has none.
	Label string
	// Detail is dimmer context on the same row, such as the directory and state.
	Detail string
	// Current marks the session this client is already attached to.
	Current bool
}

// pickAction is what happens to the chosen item.
type pickAction int

const (
	// pickSwitch moves this client to the chosen session.
	pickSwitch pickAction = iota
	// pickKill ends it, after a confirmation.
	pickKill
)

// matches returns the items the filter allows, in order.
func (p *picker) matches() []pickItem {
	if len(p.filter) == 0 {
		return p.items
	}
	needle := strings.ToLower(string(p.filter))
	var out []pickItem
	for _, it := range p.items {
		if strings.Contains(strings.ToLower(it.Label+" "+it.Detail), needle) {
			out = append(out, it)
		}
	}
	return out
}

// selected returns the item under the cursor, or false when nothing matches.
func (p *picker) selected() (pickItem, bool) {
	m := p.matches()
	if len(m) == 0 {
		return pickItem{}, false
	}
	if p.cursor >= len(m) {
		p.cursor = len(m) - 1
	}
	return m[p.cursor], true
}

// pickOutcome is what one keypress did to the picker.
type pickOutcome int

const (
	// pickedNothing means the key changed the filter or the cursor.
	pickedNothing pickOutcome = iota
	// pickedItem means enter was pressed on a match.
	pickedItem
)

// key applies one keypress.
// Escape and ctrl-c never reach here: the overlay handles going back and closing above this, so a list does
// not have to know which of the two it is in the middle of.
func (p *picker) key(k overlayKey) pickOutcome {
	switch k.Kind {
	case keyEnter:
		if _, ok := p.selected(); !ok {
			// Nothing to choose, so enter is not a choice. Silently ignored rather than closing: the user
			// has over-narrowed the filter and wants to correct it, not to start again.
			return pickedNothing
		}
		return pickedItem
	case keyUp:
		if p.cursor > 0 {
			p.cursor--
		}
	case keyDown:
		if n := len(p.matches()); p.cursor < n-1 {
			p.cursor++
		}
	case keyBackspace:
		if n := len(p.filter); n > 0 {
			p.filter = p.filter[:n-1]
			p.cursor = 0
		}
	case keyKillLine:
		p.filter = p.filter[:0]
		p.cursor = 0
	case keyRune:
		p.filter = append(p.filter, k.Rune)
		// Back to the top, since the old position means nothing in a new list.
		p.cursor = 0
	}
	return pickedNothing
}

// body renders the rows under the bar, at most limit of them.
//
// The window follows the cursor, so a list longer than the space keeps the selection visible. Without
// this the cursor walks off the bottom and the picker looks frozen.
func (p *picker) body(limit int) []string {
	if limit <= 0 {
		return nil
	}
	switch {
	case p.err != "":
		return []string{"could not list sessions: " + p.err}
	case p.loading:
		return []string{"listing sessions..."}
	}

	// Set from every session rather than from the matches, so the height reflects the list rather than
	// whatever the filter currently allows. At least one row, since a picker with no rows is invisible.
	if p.height == 0 {
		p.height = max(min(len(p.items), limit), 1)
	}
	height := min(p.height, limit)

	m := p.matches()
	if len(m) == 0 {
		return padRows([]string{"nothing matches " + string(p.filter)}, height)
	}
	if p.cursor >= len(m) {
		p.cursor = len(m) - 1
	}

	// The window is anchored so the cursor sits inside it, scrolling by whole rows.
	start := 0
	if p.cursor >= height {
		start = p.cursor - height + 1
	}
	end := min(start+height, len(m))

	// The label column is padded to the widest label *in view*, so the details line up as a column instead
	// of stepping in and out with each name's length. In view rather than overall, since a long name
	// scrolled off screen should not indent everything that is on it.
	width := 0
	for i := start; i < end; i++ {
		width = max(width, len(m[i].Label))
	}
	width = min(width, maxPickLabel)

	out := make([]string, 0, end-start)
	for i := start; i < end; i++ {
		it := m[i]
		marker := "  "
		if i == p.cursor {
			marker = "> "
		}
		here := " "
		if it.Current {
			// Marked because switching to the session you are already in is a no-op that looks like a bug.
			here = "*"
		}
		row := marker + here + it.Label
		if it.Detail != "" {
			if n := len(it.Label); n < width {
				row += strings.Repeat(" ", width-n)
			}
			row += "  " + it.Detail
		}
		// Said rather than left implicit: a window showing 3 of 20 with no sign of the rest reads as the
		// whole list.
		if i == end-1 && end < len(m) {
			row += fmt.Sprintf("   (+%d more)", len(m)-end)
		}
		out = append(out, row)
	}
	return padRows(out, height)
}

// padRows fills a list out with blank rows, so the block keeps its height.
func padRows(rows []string, height int) []string {
	for len(rows) < height {
		rows = append(rows, "")
	}
	return rows
}

// maxPickLabel bounds the label column, so one very long session name does not push every detail off the
// right of a row that has one line to work with.
const maxPickLabel = 24

// pickItemsFrom turns the server's session list into rows, marking the one this client is on.
//
// Sessions that have ended are dropped: switching to one shows a dead screen and killing one is
// meaningless, and neither is what the user reached for the picker to do.
func pickItemsFrom(resp *serverv1.ListResponse, currentID string) []pickItem {
	if resp == nil {
		return nil
	}
	out := make([]pickItem, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		if s.GetExited() {
			continue
		}
		label := s.GetName()
		if label == "" {
			// A session with no name is still choosable, and its ID is the only thing it can be called.
			// `cm run -d` and a bare `cm attach` both make these.
			label = paths.FormatSessionID(s.GetId())
		}
		out = append(out, pickItem{
			Ref:     paths.FormatSessionID(s.GetId()),
			Label:   label,
			Detail:  pickDetail(s),
			Current: s.GetId() == currentID,
		})
	}
	return out
}

// pickDetail is the dimmer half of a row: where the session is and what it is doing.
//
// Deliberately short. The full picture is what `cm list` and the TUI are for; this has one line under a
// program's screen, and a row that wraps would take a row the picker counted on.
func pickDetail(s *serverv1.Session) string {
	parts := make([]string, 0, 3)
	if dir := pickDir(s); dir != "" {
		parts = append(parts, dir)
	}
	if cmd := s.GetCommand(); cmd != "" {
		parts = append(parts, cmd)
	}
	if n := s.GetClients(); n > 0 {
		parts = append(parts, fmt.Sprintf("%d attached", n))
	}
	return strings.Join(parts, "  ")
}

// pickDir shortens a session's directory for a row.
//
// CwdIsLocal is checked rather than assumed, and that is the whole reason this is not one line: a session
// that has ssh'd elsewhere reports a path on the *other* machine, and rewriting it against this machine's
// home would claim a relationship that does not exist. internal/tui/rows.go makes the same decision for
// the same reason; the duplication is two conditions, against importing a bubbletea package into the
// client to avoid it.
func pickDir(s *serverv1.Session) string {
	cwd := s.GetCwd()
	if cwd == "" || !s.GetCwdIsLocal() {
		return cwd
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return cwd
	}
	if cwd == home {
		return "~"
	}
	if rest, found := strings.CutPrefix(cwd, home+"/"); found {
		return shortenDir("~/" + rest)
	}
	return shortenDir(cwd)
}

// shortenDir keeps the end of a path rather than the start.
//
// The tail is what distinguishes sessions: several sessions in one project share every leading segment, so
// a row cut from the right shows the same prefix on each and answers nothing. Cutting from the left is why
// this is not left to clip, which cannot know that.
func shortenDir(dir string) string {
	const limit = 28
	if len(dir) <= limit {
		return dir
	}
	// Cut at a separator so the result is a path rather than a fragment of a name.
	tail := dir[len(dir)-limit:]
	if i := strings.IndexByte(tail, '/'); i >= 0 {
		tail = tail[i:]
	}
	return "..." + tail
}
