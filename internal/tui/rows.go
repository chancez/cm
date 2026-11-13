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
// question the list is being read to answer. Then the running command, unless the row already shows the
// program's title: a title is the program's own description of the work, "Release notes for 10.0.1.0"
// where the command is "claude --resume 98c5c1d5-...", and a shell that titles itself from the command
// line would print the same words twice.
func (i item) detail(titled bool) string {
	s := i.session
	if s.ReportedDetail != "" {
		return s.ReportedDetail
	}
	if titled {
		return ""
	}
	return s.Command
}

// title is what the program in the session set as its title, or empty when it only repeats the directory.
//
// Shown because it is how a running program says what it is doing, and none of it is in the command
// line: claude titles a window with its task, gr with the PR number and branch, nvim with the file. Not
// gated on Busy, because a server that adopted a session reports it idle with no command until something
// runs, and the title is the one of the three that adoption keeps.
//
// A shell at its prompt titles itself with the directory, which where already shows: the first version of
// this row printed "~/projects/cm/.worktrees/tui  ~/p/c/.w/tui (pr/chancez/tui)", the same path twice in
// two spellings. titleNamesDir recognises those spellings.
func (i item) title() string {
	s := i.session
	if titleNamesDir(s.Title, s.Cwd) {
		return ""
	}
	return s.Title
}

// titleNamesDir reports whether title begins with a spelling of dir: the path itself, the path under
// "~", or either with each component shortened to a prefix the way fish and many zsh prompts do.
//
// Only the first word is compared, since prompts follow the path with a branch or a host. A "~" path is
// matched against dir's trailing components rather than against this machine's home, so the check holds
// for a shell that reported a directory on another host, whose home is not known here.
func titleNamesDir(title, dir string) bool {
	words := strings.Fields(title)
	if len(words) == 0 || dir == "" {
		return false
	}
	parts := strings.Split(strings.TrimSuffix(words[0], "/"), "/")
	var dirParts []string
	if trimmed := strings.Trim(dir, "/"); trimmed != "" {
		dirParts = strings.Split(trimmed, "/")
	}
	switch parts[0] {
	case "~":
		parts = parts[1:]
		if len(parts) > len(dirParts) {
			return false
		}
		dirParts = dirParts[len(dirParts)-len(parts):]
	case "":
		parts = parts[1:]
		if len(parts) != len(dirParts) {
			return false
		}
	default:
		return false
	}
	for n, part := range parts {
		if part == "" || !strings.HasPrefix(dirParts[n], part) {
			return false
		}
	}
	return true
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
// refresh moves its columns while someone is reading it, and a session gaining a name is enough to do
// that. The name column therefore elides where the table would have widened for one long cell and made
// every other row worse; paths.FormatNames drops whole names to fit rather than cutting one in half.
// nameWidth holds a set rather than one name, which is why it is 24 rather than 16. The case it is
// sized for is the one this list exists to make readable: a per-window session the terminal emulator
// named, plus the topic bound onto it, so "kitty.325,refactor" at 18. At 16 that cell had to drop the
// alias and show "kitty.325 +1", which is the auto-generated half of the pair and the useless half.
const (
	nameWidth    = 24
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

	// Every name, not just the label, so an alias bound onto a session named by the server is visible
	// rather than merely findable through the filter. paths.FormatNames drops whole names when they do
	// not fit the cell rather than cutting the joined string, which is what keeps the promise below.
	//
	// Name is the fallback, and it already reads as an ID reference for a session nothing names, because
	// the server fills it with internal/server.Label rather than with a bare name. So this cell is never
	// empty and what it shows can always be typed back into another command, which is what docs/cli.md
	// promises of the NAME column. Names is still the field to read when the names themselves are
	// wanted: see model.rename.
	name := paths.FormatNames(s.Names, nameWidth)
	if name == "" {
		name = s.Name
	}

	// Built as fixed-width cells and then trimmed, rather than assembled with a width budget. The
	// alternative computes the last column's width from a running total, which is the shape that
	// silently produces a negative width on a narrow terminal and panics inside a slice.
	head := fmt.Sprintf("%-*s %-*s %*d %*s ",
		nameWidth, truncate(name, nameWidth),
		stateWidth, truncate(it.state(), stateWidth),
		clientsWidth, s.Clients,
		ageWidth, age(time.Unix(s.CreatedAtUnix, 0), d.now),
	)

	rest := d.tail(it)

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

// tail is the row after its fixed columns: the title, the directory and the detail.
//
// They share what is left, because which of them matters depends on the session: a shell sitting
// somewhere is identified by where it is, and a session running something is identified by what. None is
// worth a fixed column that is usually empty.
//
// The title first. After the directory it was cut off the right edge: the fixed columns take 45, and a
// worktree path such as ~/projects/work/hubble-timescape/.worktrees/gitattributes_generated is 67 more.
// A shell at its prompt has no title here, so its row still starts with the directory.
func (d delegate) tail(it item) string {
	where, title := it.where(d.home), it.title()
	if where == "" {
		// No local directory to show, which means the shell reported one on another host. Its title is
		// then the only thing left that says anything about where it is, so it takes the directory's
		// place even when it only names that directory.
		where, title = it.session.Title, ""
	}
	var parts []string
	for _, part := range []string{title, where, it.detail(title != "")} {
		if part != "" {
			parts = append(parts, part)
		}
	}
	return strings.Join(parts, "  ")
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

// Match is the picker's filter as a function: the sessions "/" would leave for query, best match first.
//
// Exported so `cm search` prints exactly what `cm search --tui` opens on. Built from the list's own pieces
// rather than reimplemented: the same FilterValue, the same rows order underneath, and bubbles' own
// DefaultFilter, which ranks with a stable sort, so ties fall back to the order rows gives them in both.
func Match(query string, sessions []*serverv1.Session) []*serverv1.Session {
	items := rows(sessions)
	targets := make([]string, len(items))
	for i, it := range items {
		targets[i] = it.FilterValue()
	}
	ranks := list.DefaultFilter(query, targets)
	out := make([]*serverv1.Session, 0, len(ranks))
	for _, r := range ranks {
		out = append(out, items[r.Index].(item).session)
	}
	return out
}
