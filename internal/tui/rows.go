package tui

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

	"charm.land/bubbles/v2/list"
	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// item is one session in the list.
type item struct {
	session *serverv1.Session
}

// ref is how to name this session to the server.
//
// Always the ID, never a name, and that is the same rule docs/cli.md gives a script: a name is a
// binding and can be pointed at another session at any time, including between the refresh that drew
// this row and the key that acts on it. An ID either finds the session that was on screen or fails,
// and failing is the right outcome for a row describing something that is gone. internal/server.
// Manager.Open is explicit that a stale ID is refused rather than created, so a keypress on a row for
// a killed session reports it instead of silently making a new one.
func (i item) ref() string { return paths.FormatSessionID(i.session.Id) }

// FilterValue is what "/" searches.
//
// Everything a person might remember the session by, not just its name. Half of these sessions are
// named by the server, so they are called "s17" and there is nothing to type; what the user remembers
// is the directory, the command that is running, or the tag they grouped it under.
func (i item) FilterValue() string {
	s := i.session
	fields := make([]string, 0, 6+len(s.Names)+len(s.Tags))
	fields = append(fields, s.Name, s.Id, s.Command, s.Title, s.Cwd, s.ReportedDetail)
	fields = append(fields, s.Names...)
	for k, v := range s.Tags {
		if v == "" {
			fields = append(fields, k)
			continue
		}
		fields = append(fields, k+"="+v)
	}
	return strings.Join(fields, " ")
}

// state is the short word for what this session is doing.
//
// Deliberately not `cm ls`'s STATE column, which packs the reported detail and the running command
// into one cell because a table has to fit them somewhere. A list has a whole line, so the state and
// what produced it are separate columns: one is scanned down, the other is read across. See detail.
//
// Everything here is derived from what the server sent, which means it is only as good as the
// server's own view. A restarted server currently reports an empty command and no reported state for
// sessions it adopted, so those rows read "idle" until something runs.
func (i item) state() string {
	s := i.session
	switch {
	case s.State == serverv1.SessionState_SESSION_STATE_DEAD:
		// Not "exited": the shim could not be reached, so the outcome is unknown rather than
		// observed, and ExitCode means nothing.
		return "dead"
	case s.State == serverv1.SessionState_SESSION_STATE_EXITED || s.Exited:
		return fmt.Sprintf("exited(%d)", s.ExitCode)
	case s.ReportedState != "":
		// A report from the program itself wins over anything derived from bytes going past. It is
		// the only thing that can say "blocked", which is the state a person scanning this list is
		// looking for.
		return s.ReportedState
	case s.Busy:
		return "busy"
	}
	return "idle"
}

// detail is what the session is doing, in words the session supplied.
//
// The reported detail first, because a program that bothered to say "needs approval" is answering the
// question the list is being read to answer. Then the running command, then the terminal title, which
// is what a shell with nothing running still says about itself.
func (i item) detail() string {
	s := i.session
	switch {
	case s.ReportedDetail != "":
		return s.ReportedDetail
	case s.Command != "":
		return s.Command
	}
	return s.Title
}

// where is the session's directory, abbreviated the way a prompt does.
//
// Empty for a session whose shell reported a directory on another host, because this machine's home
// is not that machine's and rewriting it would claim a relationship that does not exist. CwdIsLocal
// is the server's answer to that question, so it is not re-derived here.
func (i item) where(home string) string {
	s := i.session
	if s.Cwd == "" || !s.CwdIsLocal {
		return ""
	}
	home = strings.TrimSuffix(home, "/")
	if home == "" || home == "/" {
		return s.Cwd
	}
	if s.Cwd == home {
		return "~"
	}
	if rest, found := strings.CutPrefix(s.Cwd, home+"/"); found {
		return "~/" + rest
	}
	return s.Cwd
}

// age renders how long ago t was, in one or two characters plus a unit.
//
// now is a parameter rather than time.Now so a test asserting a row does not have to run inside the
// same second it was written.
func age(t, now time.Time) string {
	d := now.Sub(t)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// Column widths for everything except the last column, which takes the rest of the line.
//
// Fixed rather than measured against the contents, unlike `cm ls`. A list that re-measures on every
// refresh moves its columns while someone is reading it, and a session name changing length is enough
// to do that. A name is capped at 24 by paths.ValidateSessionName, so the name column only truncates
// where the table would have widened for one long name and made every other row worse.
const (
	nameWidth    = 16
	stateWidth   = 10
	clientsWidth = 3
	ageWidth     = 4
)

// delegate renders one session per line.
type delegate struct {
	// home abbreviates directories under it. Captured once rather than read per row, and passed in
	// rather than looked up, so a test does not have to set HOME for the process.
	home string
	// now is what ages are measured against, and is the time of the last refresh rather than of the
	// render. Every row in a frame then agrees, which a render-time clock does not guarantee: a
	// paint that straddles a second shows two sessions created together as "0s" and "1s".
	now time.Time
}

func (delegate) Height() int                         { return 1 }
func (delegate) Spacing() int                        { return 0 }
func (delegate) Update(tea.Msg, *list.Model) tea.Cmd { return nil }

var (
	// Reverse rather than a color for the selected row, so the picker inherits whatever the terminal
	// is themed as instead of guessing at a palette that may be unreadable in it.
	selectedStyle = lipgloss.NewStyle().Reverse(true)
	// Faint for the columns that are context rather than identity. A row is scanned by name and
	// state; the rest is read once the eye has stopped.
	faintStyle = lipgloss.NewStyle().Faint(true)
)

// Render writes one row.
func (d delegate) Render(w io.Writer, m list.Model, index int, listItem list.Item) {
	it, ok := listItem.(item)
	if !ok {
		return
	}
	s := it.session

	// Name already reads as an ID reference for a session nothing names, because the server fills it
	// with internal/server.Label rather than with a bare name. So this cell is never empty and what it
	// shows can always be typed back into another command, which is what docs/cli.md promises of the
	// NAME column. Names is the field to read when the real names are wanted: see model.rename.
	name := s.Name

	// Built as fixed-width cells and then trimmed, rather than assembled with a width budget. The
	// alternative computes the last column's width from a running total, which is the shape that
	// silently produces a negative width on a narrow terminal and panics inside a slice.
	head := fmt.Sprintf("%-*s %-*s %*d %*s ",
		nameWidth, truncate(name, nameWidth),
		stateWidth, truncate(it.state(), stateWidth),
		clientsWidth, s.Clients,
		ageWidth, age(time.Unix(s.CreatedAtUnix, 0), d.now),
	)

	// The directory and the detail share what is left, because which of them matters depends on the
	// session: a shell sitting somewhere is identified by where it is, and a session running something
	// is identified by what. Neither is worth a fixed column that is usually empty.
	rest := it.where(d.home)
	if detail := it.detail(); detail != "" {
		if rest != "" {
			rest += "  "
		}
		rest += detail
	}

	line := head + rest
	if width := m.Width(); width > 0 {
		line = truncate(line, width)
		if index == m.Index() {
			// Padded to the full width before styling, so the selected row reads as a bar across the
			// list rather than as a highlight that stops at the last character.
			line = fmt.Sprintf("%-*s", width, line)
		}
	}
	if index == m.Index() {
		fmt.Fprint(w, selectedStyle.Render(line))
		return
	}
	// Only the tail is faint. Dimming the whole row would dim the name and the state, which are the
	// two things the list exists to show.
	fmt.Fprint(w, head+faintStyle.Render(rest))
}

// truncate shortens s to at most width cells, marking that it was cut.
//
// Cut with an ASCII ".." rather than a single-character ellipsis, because plain ASCII is the rule
// here and because a multi-byte glyph in a fixed-width cell is one more thing that can disagree with
// the terminal about how wide it is.
func truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}
	if lipgloss.Width(s) <= width {
		return s
	}
	if width <= 2 {
		return strings.Repeat(".", width)
	}
	// Sliced by rune rather than by byte, so a path with a non-ASCII component is not cut in the
	// middle of one and rendered as a replacement character.
	runes := []rune(s)
	for n := min(len(runes), width); n > 0; n-- {
		candidate := string(runes[:n-2]) + ".."
		if lipgloss.Width(candidate) <= width {
			return candidate
		}
	}
	return ".."
}

// rows turns a server's answer into list items, in the order `cm ls` prints them.
//
// The same order deliberately: someone who has just read `cm ls` should find the rows where they
// were. Oldest first with names breaking ties is also stable between refreshes, which matters more
// here than in a one-shot listing, because an unstable order moves a row out from under the cursor
// while it is being aimed at.
func rows(sessions []*serverv1.Session) []list.Item {
	sorted := make([]*serverv1.Session, len(sessions))
	copy(sorted, sessions)
	sort.SliceStable(sorted, func(i, j int) bool {
		if sorted[i].CreatedAtUnix != sorted[j].CreatedAtUnix {
			return sorted[i].CreatedAtUnix < sorted[j].CreatedAtUnix
		}
		return sorted[i].Name < sorted[j].Name
	})
	items := make([]list.Item, 0, len(sorted))
	for _, s := range sorted {
		items = append(items, item{session: s})
	}
	return items
}
