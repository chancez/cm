package tui

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"charm.land/bubbles/v2/help"
	"charm.land/bubbles/v2/key"
	"charm.land/bubbles/v2/list"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// mode is what the picker is currently asking for.
//
// A mode rather than a stack of sub-models, because there are two questions and both are one
// keystroke deep. What matters is that a mode captures what it acts on when it is entered: see target.
type mode int

const (
	// modeList is the list, where the keys act on the selected session.
	modeList mode = iota
	// modeConfirmKill is waiting for y or n on a kill.
	modeConfirmKill
	// modeRename is reading a new name.
	modeRename
)

type model struct {
	// ctx bounds every request and every attachment, so quitting or a signal stops all of them.
	ctx      context.Context
	sessions Sessions
	attach   AttachFunc
	tags     []string
	home     string

	list  list.Model
	keys  keyMap
	help  help.Model
	input textinput.Model

	mode mode
	// target is the session a confirmation or a rename acts on, captured when the mode was entered
	// rather than read back from the list when the answer arrives.
	//
	// This is the bug that would otherwise be waiting here. The list refreshes on a timer, and a
	// refresh reorders nothing but does move the cursor when a session ends and a row disappears. So
	// between pressing x and pressing y the selection can be a different session, and a kill that
	// re-read the selection would end the wrong one. Capturing makes the confirmation say what it will
	// do and then do exactly that.
	target *serverv1.Session

	// handoff is the attachment the terminal was last handed to, or nil. See model.handOff.
	handoff *attachCommand

	// status is the last thing worth saying: what an attachment did, or what an action changed.
	//
	// Held in the model and rendered as part of the frame, never printed. Anything written straight to
	// the terminal lands on a screen bubbletea is about to repaint. See the package comment.
	status string
	// err is the last failure, shown until something succeeds.
	err error

	// loaded records that a list has arrived, so an empty list reads as "no sessions" rather than as
	// "still asking". Without it a fresh start shows "no sessions" for as long as the first request
	// takes, which is the one moment the answer is not known.
	loaded bool

	width  int
	height int
}

func newModel(ctx context.Context, opts Options) model {
	// home is read once. It cannot change under a running process in any way that matters, and
	// reading it per row would put an environment lookup in the render path.
	home, _ := os.UserHomeDir()

	input := textinput.New()
	input.Prompt = "name: "
	// A name is capped at this by paths.ValidateSessionName, so the field refuses what the server
	// would refuse anyway, before a request is spent learning it.
	input.CharLimit = 24

	l := list.New(nil, delegate{home: home, now: time.Now()}, 0, 0)
	l.Title = "sessions"
	// The list's own help line is suppressed because the picker renders one that covers both its
	// bindings and the list's. Two help lines disagreeing about what a key does is worse than either.
	l.SetShowHelp(false)

	return model{
		ctx:      ctx,
		sessions: opts.Sessions,
		attach:   opts.Attach,
		tags:     opts.Tags,
		home:     home,
		list:     l,
		keys:     defaultKeys(),
		help:     help.New(),
		input:    input,
		// The notice occupies the status line until something replaces it, which is the right lifetime:
		// it is worth reading once and then out of the way.
		status: opts.Notice,
	}
}

// listedMsg is a completed List.
type listedMsg struct {
	sessions []*serverv1.Session
	err      error
}

// refreshMsg is the timer asking for another List.
type refreshMsg struct{}

// killedMsg is a completed Kill.
type killedMsg struct {
	// label is what to call the session in a message, captured before the kill because a killed
	// session cannot be asked what it was called.
	label string
	err   error
}

// renamedMsg is a completed rename.
type renamedMsg struct {
	label string
	name  string
	// dropped is the name the rename took off the session, or empty when it had none to drop.
	dropped string
	err     error
}

func (m model) Init() tea.Cmd {
	// The first list is fetched rather than waited for, so the picker is useful in one round trip
	// instead of one refresh interval.
	return m.fetch()
}

// fetch asks the server what exists.
func (m model) fetch() tea.Cmd {
	return func() tea.Msg {
		resp, err := m.sessions.List(m.ctx, &serverv1.ListRequest{Tags: m.tags})
		if err != nil {
			return listedMsg{err: err}
		}
		return listedMsg{sessions: resp.Sessions}
	}
}

// schedule asks for the next refresh.
//
// Chained off each answer rather than run as an independent ticker, which is what keeps a slow or
// hanging server from queueing requests behind each other: there is never more than one in flight.
func (m model) schedule() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return refreshMsg{} })
}

func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case listedMsg:
		return m.applyList(msg)

	case refreshMsg:
		return m, m.fetch()

	case attachedMsg:
		m.err = msg.err
		m.status = describeAttachment(msg)
		// Refreshed at once rather than at the next tick. The session just left has almost certainly
		// changed: its client count dropped, and if its shell exited the row should go.
		return m, m.fetch()

	case killedMsg:
		m.err = msg.err
		if msg.err == nil {
			m.status = "killed " + msg.label
		}
		return m, m.fetch()

	case renamedMsg:
		m.err = msg.err
		if msg.err == nil {
			m.status = fmt.Sprintf("%s is now %s", msg.label, msg.name)
			if msg.dropped != "" {
				m.status += ", and no longer " + msg.dropped
			}
		}
		return m, m.fetch()

	case tea.KeyPressMsg:
		return m.key(msg)
	}

	// Everything else belongs to the list: its spinner, its filter's cursor, its paginator.
	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// applyList replaces the rows, keeping the cursor on the session it was on.
func (m model) applyList(msg listedMsg) (tea.Model, tea.Cmd) {
	if msg.err != nil {
		// The old rows are left on screen. They are stale, but a server that has gone away is usually
		// coming back, and blanking the list loses the user's place for what may be one failed poll.
		// The error says the rows cannot be trusted.
		m.err = msg.err
		return m, m.schedule()
	}
	m.err = nil
	m.loaded = true

	// Which session the cursor is on, not which row: the row a session sits on changes whenever
	// anything older than it ends. Restoring by index is what makes a list under a timer jump.
	var selected string
	if it, ok := m.selected(); ok {
		selected = it.session.Id
	}

	// The delegate is rebuilt so every age in the frame is measured from this answer rather than from
	// the render. See delegate.now.
	m.list.SetDelegate(delegate{home: m.home, now: time.Now()})
	cmd := m.list.SetItems(rows(msg.sessions))

	if selected != "" {
		// Searched over the visible items, which is what the cursor indexes: with a filter applied,
		// index 3 is the third match rather than the third session.
		for i, listItem := range m.list.VisibleItems() {
			if it, ok := listItem.(item); ok && it.session.Id == selected {
				m.list.Select(i)
				break
			}
		}
	}
	return m, tea.Batch(cmd, m.schedule())
}

// key routes a keypress to whatever is currently asking a question.
func (m model) key(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch m.mode {
	case modeConfirmKill:
		return m.confirmKey(msg)
	case modeRename:
		return m.renameKey(msg)
	}

	// While a filter is being typed every key belongs to it, checked before the picker's own bindings
	// rather than after. Otherwise typing a session name into the filter runs commands: "n" would
	// create a session, "x" would offer to kill one, and "q" would quit in the middle of a word.
	if m.list.FilterState() == list.Filtering {
		var cmd tea.Cmd
		m.list, cmd = m.list.Update(msg)
		return m, cmd
	}

	switch {
	case key.Matches(msg, m.keys.Quit):
		return m, tea.Quit

	case key.Matches(msg, m.keys.Help):
		m.help.ShowAll = !m.help.ShowAll
		// The list is resized because the help got taller or shorter, and the list's height is what
		// pays for it. Without this the expanded help draws over the last rows.
		m.layout()
		return m, nil

	case key.Matches(msg, m.keys.New):
		// No name and no prompt: the server allocates one, which is what `cm attach` with no argument
		// does and what a new terminal window gets. A session worth naming is named with "r" once it
		// is clear what it is for, which is the order the naming usually happens in anyway.
		return m.handOff("")

	case key.Matches(msg, m.keys.Attach):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		return m.handOff(it.ref())

	case key.Matches(msg, m.keys.Kill):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.mode = modeConfirmKill
		m.target = it.session
		return m, nil

	case key.Matches(msg, m.keys.Rename):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.mode = modeRename
		m.target = it.session
		m.input.SetValue("")
		return m, m.input.Focus()
	}

	var cmd tea.Cmd
	m.list, cmd = m.list.Update(msg)
	return m, cmd
}

// confirmKey answers a kill confirmation.
//
// Only "y" kills. Anything else cancels, rather than only a listed cancel key: a stray keypress at a
// prompt that ends a shell should do nothing, and there is no keystroke worth guessing at here.
func (m model) confirmKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	target := m.target
	m.mode = modeList
	m.target = nil
	if msg.String() != "y" {
		m.status = "left it running"
		return m, nil
	}
	return m, m.kill(target)
}

// renameKey reads a new name.
func (m model) renameKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case "esc":
		m.mode = modeList
		m.target = nil
		return m, nil
	case "enter":
		name := strings.TrimSpace(m.input.Value())
		target := m.target
		m.mode = modeList
		m.target = nil
		if name == "" {
			// An empty name is a cancel rather than an error. It is what enter on an untouched field
			// means, and unbinding every name is not something anybody reaches for by pressing enter.
			return m, nil
		}
		// Validated here so a bad name is refused without spending a request, and with the same message
		// the rest of cm gives: the rule that a name may not contain the ID sigil is what keeps the two
		// kinds of reference from being confused, and it is worth stating identically wherever it is
		// enforced.
		if err := paths.ValidateSessionName(name); err != nil {
			m.err = err
			return m, nil
		}
		return m, m.rename(target, name)
	}
	var cmd tea.Cmd
	m.input, cmd = m.input.Update(msg)
	return m, cmd
}

// kill ends a session.
func (m model) kill(target *serverv1.Session) tea.Cmd {
	if target == nil {
		return nil
	}
	label := target.Name
	ref := paths.FormatSessionID(target.Id)
	return func() tea.Msg {
		resp, err := m.sessions.Kill(m.ctx, &serverv1.KillRequest{Sessions: []string{ref}})
		if err != nil {
			return killedMsg{label: label, err: err}
		}
		// A Kill can fail per session while the call succeeds, and reporting only the call would show
		// "killed work" for a session that is still running.
		if failure, ok := resp.Errors[ref]; ok {
			return killedMsg{label: label, err: fmt.Errorf("killing %s: %s", label, failure)}
		}
		return killedMsg{label: label}
	}
}

// rename points a name at a session and takes off the one it had.
//
// A bind plus an unbind because a name is a binding rather than a field on the session, and cm has no
// rename RPC: docs/cli.md describes renaming as exactly these two steps. Doing them here rather than
// asking for a third spelling on the server keeps one meaning per request.
//
// Only a session that had exactly one name loses it. Several names are deliberate, since a window can
// borrow a session that something else also names, so choosing one to remove would be a guess. Names
// is read rather than Name, which holds an ID reference for a session nothing names and would
// otherwise be unbound as though it were one.
func (m model) rename(target *serverv1.Session, name string) tea.Cmd {
	if target == nil {
		return nil
	}
	label := target.Name
	ref := paths.FormatSessionID(target.Id)
	var dropped string
	if len(target.Names) == 1 && target.Names[0] != name {
		dropped = target.Names[0]
	}
	return func() tea.Msg {
		if _, err := m.sessions.Bind(m.ctx, &serverv1.BindRequest{Name: name, Session: ref}); err != nil {
			return renamedMsg{label: label, name: name, err: err}
		}
		if dropped == "" {
			return renamedMsg{label: label, name: name}
		}
		if _, err := m.sessions.Unbind(m.ctx, &serverv1.UnbindRequest{Name: dropped}); err != nil {
			// The new name is bound and the old one is not gone, which is a session with two names
			// rather than a failed rename. Said plainly, because the state is fine and the user only
			// needs to know the old name still works.
			return renamedMsg{
				label: label,
				name:  name,
				err:   fmt.Errorf("%s is now %s, but %s still names it too: %w", label, name, dropped, err),
			}
		}
		return renamedMsg{label: label, name: name, dropped: dropped}
	}
}

// selected is the session under the cursor, and false when the list is empty or filtered to nothing.
func (m model) selected() (item, bool) {
	it, ok := m.list.SelectedItem().(item)
	return it, ok
}

// layout gives the list whatever height the rest of the frame does not need.
//
// Measured from the rendered strings rather than from a count of lines this is expected to produce,
// because the help and the status line both change height: the help expands, and a long error wraps.
func (m *model) layout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	chrome := lineCount(m.footer())
	height := m.height - chrome
	if height < 1 {
		height = 1
	}
	m.list.SetSize(m.width, height)
}

func (m model) View() tea.View {
	body := m.list.View() + "\n" + m.footer()
	v := tea.NewView(body)
	// The alternate screen, so quitting leaves the shell's scrollback as it was found. A picker that
	// scrolls the terminal's history away is one people stop opening.
	v.AltScreen = true
	return v
}

// footer is everything under the list: the prompt or status, then the help.
func (m model) footer() string {
	var line string
	switch {
	case m.mode == modeConfirmKill && m.target != nil:
		// The session is named in the question rather than left as "this one", because the answer is
		// one keystroke and the list is still on screen behind it with a cursor that may have moved.
		line = fmt.Sprintf("kill %s? [y/N]", m.target.Name)
	case m.mode == modeRename && m.target != nil:
		line = fmt.Sprintf("rename %s to %s", m.target.Name, m.input.View())
	case m.err != nil:
		line = "error: " + m.err.Error()
	case m.status != "":
		line = m.status
	case !m.loaded:
		line = "asking the server..."
	case len(m.list.Items()) == 0:
		line = "no sessions. press n to start one"
	}
	return line + "\n" + m.help.View(m.fullHelp())
}

// fullHelp is the picker's bindings plus the list's own.
//
// Combined here because only the model can ask the list what its keys are, and a help line that
// leaves out "/" to filter is describing a different program from the one on screen.
func (m model) fullHelp() help.KeyMap {
	return combinedHelp{picker: m.keys, list: m.list}
}

type combinedHelp struct {
	picker keyMap
	list   list.Model
}

// ShortHelp is the picker's keys followed by the list's filter and navigation.
func (c combinedHelp) ShortHelp() []key.Binding {
	return append(c.picker.ShortHelp(), c.list.ShortHelp()...)
}

func (c combinedHelp) FullHelp() [][]key.Binding {
	return append(c.picker.FullHelp(), c.list.FullHelp()...)
}

// describeAttachment says how an attachment ended.
//
// The wording matches what `cm attach` prints for the same outcomes, so the two do not read as
// different tools reporting the same event. What differs is where it goes: this is a string in the
// frame, and printing it would corrupt the repaint.
func describeAttachment(msg attachedMsg) string {
	switch {
	case msg.err != nil:
		return ""
	case msg.attachment.Stale:
		// Said in full rather than as "upgrade available", because the user has to act and the action
		// is not obvious: this process is the old build and only restarting it replaces one.
		return fmt.Sprintf("detached from %s. the server is newer than this picker: quit and reopen it",
			msg.attachment.Session)
	case msg.attachment.Exited && msg.attachment.ExitCode < 0:
		// A negative code means the shim became unreachable rather than the shell reporting a status,
		// so there is no exit code worth showing.
		return fmt.Sprintf("session %s ended unexpectedly", msg.attachment.Session)
	case msg.attachment.Exited:
		return fmt.Sprintf("session %s ended (exit %d)", msg.attachment.Session, msg.attachment.ExitCode)
	case msg.attachment.Detached:
		return "detached from " + msg.attachment.Session
	}
	return ""
}

// lineCount counts the lines a rendered string occupies.
//
// Its own function rather than lipgloss.Height so the empty string counts as zero lines instead of
// one, which is what the layout arithmetic needs: a footer that is not there must not take a row.
func lineCount(s string) int {
	if s == "" {
		return 0
	}
	return strings.Count(s, "\n") + 1
}
