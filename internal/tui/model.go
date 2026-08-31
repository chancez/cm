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
	// switchTo moves whoever opened the picker, or is nil when there is nobody to move. See SwitchFunc.
	switchTo SwitchFunc
	tags     []string
	home     string
	// interval is how often the list re-reads the server, zero for not at all. See Options.Refresh.
	interval time.Duration

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

	// preview is the output pane under the list. See syncPreview for how it is kept in step with the
	// selection.
	preview preview

	// status is the last thing worth saying: what an attachment did, or what an action changed.
	//
	// Held in the model and rendered as part of the frame, never printed. Anything written straight to
	// the terminal lands on a screen bubbletea is about to repaint. See the package comment.
	status string
	// err is the last failure from something the user did: a kill, a rename, an attachment.
	//
	// Separate from listErr, and the separation is a bug fix rather than tidiness. Every action asks for
	// a refresh immediately afterwards so the list reflects it, and while one error field served both, a
	// successful refresh cleared the action's error microseconds after it was set. A failed kill reported
	// nothing at all, and the only reason that was ever seen is a test harness faithful enough to run the
	// refresh the runtime would have run.
	err error
	// listErr is the last failure from the poll, which the refresh that follows owns and clears.
	listErr error

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
	// No prompt of its own: the footer already says "rename work to" and the field follows it, so a
	// prompt here rendered "rename work to name: notebook".
	input.Prompt = ""
	// A name is capped at this by paths.ValidateSessionName, so the field refuses what the server
	// would refuse anyway, before a request is spent learning it.
	input.CharLimit = 24

	l := list.New(nil, delegate{home: home, now: time.Now()}, 0, 0)
	l.Title = "sessions"
	// The list's own help line is suppressed because the picker renders one that covers both its
	// bindings and the list's. Two help lines disagreeing about what a key does is worse than either.
	l.SetShowHelp(false)

	interval := refreshInterval
	if opts.Refresh != nil {
		interval = *opts.Refresh
	}

	keys := defaultKeys()
	// Enabled only when the caller can actually switch. A disabled binding is skipped by key.Matches and
	// left out of the help, so the key is neither offered nor inert.
	if opts.Switch != nil {
		keys.Switch.SetEnabled(true)
	}

	return model{
		ctx:      ctx,
		interval: interval,
		sessions: opts.Sessions,
		attach:   opts.Attach,
		switchTo: opts.Switch,
		tags:     opts.Tags,
		home:     home,
		list:     l,
		keys:     keys,
		help:     help.New(),
		input:    input,
		preview:  preview{on: opts.Preview},
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
	if m.interval <= 0 {
		return nil
	}
	return tea.Tick(m.interval, func(time.Time) tea.Msg { return refreshMsg{} })
}

// Update handles a message and then reconciles the preview pane with whatever is selected afterwards.
//
// The reconciliation is here, once, rather than in each branch that can move the cursor. The selection
// changes on a keypress, on a refresh that removed a row, on a filter being typed and on a filter being
// cleared, and a request issued from only some of those is the version of this that looks like it works.
func (m model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	next, cmd := m.update(msg)
	next, previewCmd := next.syncPreview()
	if previewCmd == nil {
		return next, cmd
	}
	return next, tea.Batch(cmd, previewCmd)
}

func (m model) update(msg tea.Msg) (model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.layout()
		return m, nil

	case listedMsg:
		return m.applyList(msg)

	case previewMsg:
		// A late answer for a session that is no longer selected is dropped rather than shown. The
		// request was in flight while the cursor moved, so painting it now would put one session's
		// output under another session's row, which is a confident wrong answer rather than a gap.
		if msg.sessionID != m.preview.want {
			return m, nil
		}
		m.preview.have = msg.sessionID
		m.preview.err = msg.err
		m.preview.lines = previewLines(msg.data, m.preview.width, m.previewContentHeight())
		return m, nil

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

	case switchedMsg:
		if msg.err != nil {
			m.err = msg.err
			return m, nil
		}
		// Nothing to report and nothing to refresh: the picker is done. The caller moves the window as this
		// process exits, so a status line written here would be painted and then thrown away with the frame.
		return m, tea.Quit

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
func (m model) applyList(msg listedMsg) (model, tea.Cmd) {
	if msg.err != nil {
		// The old rows are left on screen. They are stale, but a server that has gone away is usually
		// coming back, and blanking the list loses the user's place for what may be one failed poll.
		// The error says the rows cannot be trusted.
		m.listErr = msg.err
		return m, m.schedule()
	}
	m.listErr = nil
	m.loaded = true
	// The selected session is still running, so its last lines have moved even when the selection has
	// not. Marked here rather than fetched, so one place decides whether a fetch happens at all.
	m.preview.due = true

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
	// Re-laid out because the split depends on how many rows there are, and the first layout happened
	// before any had arrived. Without this the list keeps the height it was given when it was empty: it
	// showed one session out of three, with a pagination indicator, and the pane below it looked fine.
	m.layout()

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
func (m model) key(msg tea.KeyPressMsg) (model, tea.Cmd) {
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

	case key.Matches(msg, m.keys.Preview):
		m.preview.on = !m.preview.on
		if !m.preview.on {
			// Dropped rather than kept for next time. Content held while the pane is closed is content
			// that will be stale when it reopens, and the reopen fetches anyway.
			m.preview = preview{on: false}
		}
		m.layout()
		return m, nil

	case key.Matches(msg, m.keys.HalfPageDown):
		m.moveCursor(m.halfPage())
		return m, nil

	case key.Matches(msg, m.keys.HalfPageUp):
		m.moveCursor(-m.halfPage())
		return m, nil

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

	case key.Matches(msg, m.keys.Switch):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.err = nil
		return m, m.switchHere(it)

	case key.Matches(msg, m.keys.Kill):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		// The previous action's error goes when a new action begins, rather than on the next refresh.
		// It describes something that has been read by now, and leaving it up would put it over the
		// question being asked.
		m.err = nil
		m.mode = modeConfirmKill
		m.target = it.session
		return m, nil

	case key.Matches(msg, m.keys.Rename):
		it, ok := m.selected()
		if !ok {
			return m, nil
		}
		m.err = nil
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
func (m model) confirmKey(msg tea.KeyPressMsg) (model, tea.Cmd) {
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
func (m model) renameKey(msg tea.KeyPressMsg) (model, tea.Cmd) {
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

// switchedMsg is a completed switch.
type switchedMsg struct {
	label string
	err   error
}

// switchHere moves the caller to a session.
//
// By ID rather than by the name on the row, for the reason the overlay's own picker uses one: the list
// refreshes on a timer and a name can be bound to another session between the row being drawn and the key
// being pressed, which would send the window somewhere the user did not choose.
func (m model) switchHere(it item) tea.Cmd {
	label := it.session.GetName()
	ref := it.ref()
	switchTo := m.switchTo
	return func() tea.Msg {
		return switchedMsg{label: label, err: switchTo(ref)}
	}
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

// halfPage is how many rows ctrl-u and ctrl-d move.
//
// Asked of the paginator rather than derived from the window height, which is the same reason layout
// asks it: PerPage is how many rows the list decided it can show, so the output pane being open and the
// help being expanded are already paid for. A half page of at least one row, so a list too short to
// paginate still moves rather than swallowing the key.
func (m model) halfPage() int {
	half := m.list.Paginator.PerPage / 2
	if half < 1 {
		return 1
	}
	return half
}

// moveCursor steps the cursor n rows, negative for up.
//
// Stepped one row at a time rather than jumped, because the list is paginated rather than scrolled:
// CursorUp and CursorDown turn the page when they run off the end of one, so a loop over them walks the
// whole list, while setting an index would leave the paginator on the page it was on. Both stop at the
// ends, so a half page from the last row lands on the last row instead of wrapping.
func (m *model) moveCursor(n int) {
	for ; n > 0; n-- {
		m.list.CursorDown()
	}
	for ; n < 0; n++ {
		m.list.CursorUp()
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
	avail := m.height - chrome
	if avail < 1 {
		avail = 1
	}

	// The pane gets its share first, and can end up with none: a window too short for both keeps the
	// list, since a picker without a list is not a picker.
	m.preview.width = m.width
	share := 0
	if m.preview.on {
		share = previewHeightFor(avail)
	}

	rows := avail - share
	if rows < 1 {
		rows = 1
	}
	m.list.SetSize(m.width, rows)

	// The list is never given more room than it has rows to put in it, and the pane takes what is left
	// over. bubbles pads a list to whatever height it is told, so a picker with three sessions on a tall
	// window drew ten blank lines between the last row and the pane. Handing that space to the pane
	// instead shows more of the session being looked at, which is what the space is for.
	//
	// What the list spends on itself is asked for rather than assumed: PerPage is how many rows it decided
	// it could show at this height, so the difference is its title, its count and their blank lines. The
	// first version of this guessed four and was wrong, and a wrong guess here does not look like a bad
	// constant, it looks like a list that shows one session out of three.
	if share > 0 && m.list.Paginator.PerPage > 0 {
		chrome := rows - m.list.Paginator.PerPage
		if needed := len(m.list.Items()) + chrome; needed < rows {
			rows = needed
			m.list.SetSize(m.width, rows)
		}
	}
	m.preview.height = 0
	if share > 0 {
		m.preview.height = avail - rows
	}
}

// previewContentHeight is how many lines of session output the pane can hold, its header aside.
func (m model) previewContentHeight() int {
	if m.preview.height <= 1 {
		return 0
	}
	return m.preview.height - 1
}

// syncPreview keeps the pane pointed at the selected session, and asks for its output when needed.
//
// Returns a command only when something has to be read, which is when the selection changed or a
// refresh marked the content stale. Nothing is read while the pane is closed, so a picker with no pane
// costs exactly what it did before this existed.
func (m model) syncPreview() (model, tea.Cmd) {
	if !m.preview.on || m.previewContentHeight() == 0 {
		return m, nil
	}
	it, ok := m.selected()
	if !ok {
		// Nothing is selected, which is an empty list or a filter matching nothing. The pane is emptied
		// rather than left showing the last session, which is no longer on screen to be pointed at.
		m.preview = preview{on: true, height: m.preview.height, width: m.preview.width}
		return m, nil
	}

	if id := it.session.Id; id != m.preview.want {
		// Cleared as well as re-aimed. Keeping the old lines until the new ones arrive would show one
		// session's output under another's row for as long as the request takes, which is the same
		// mistake as accepting a late answer, arrived at from the other direction.
		m.preview.want = id
		m.preview.have = ""
		m.preview.lines = nil
		m.preview.err = nil
		m.preview.due = true
	}
	if !m.preview.due {
		return m, nil
	}
	m.preview.due = false
	return m, m.fetchPreview(it.session.Id, it.ref(), m.previewContentHeight())
}

func (m model) View() tea.View {
	body := m.list.View()
	if pane := m.preview.view(m.previewLabel()); pane != "" {
		body += "\n" + pane
	}
	body += "\n" + m.footer()
	v := tea.NewView(body)
	// The alternate screen, so quitting leaves the shell's scrollback as it was found. A picker that
	// scrolls the terminal's history away is one people stop opening.
	v.AltScreen = true
	return v
}

// previewLabel names the session the pane is showing, for its header.
//
// Read from what the pane holds rather than from the selection, so a pane still waiting on a request is
// labelled with the session it is waiting for and never with one whose content it does not have.
func (m model) previewLabel() string {
	for _, listItem := range m.list.Items() {
		if it, ok := listItem.(item); ok && it.session.Id == m.preview.want {
			return it.session.Name
		}
	}
	return ""
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
	case m.listErr != nil:
		line = "error: " + m.listErr.Error()
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

// FullHelp is the picker's columns followed by the list's, with the half page keys in the list's
// navigation column rather than in a column of their own.
//
// That placement is a width decision, measured in a 100 column window: the expanded help is not
// truncated to the window, it already came within 11 columns of the edge, and a seventh column pushed
// the filter and quit columns off it entirely. The navigation column is also where they read best, next
// to the whole page keys they are the half page version of.
func (c combinedHelp) FullHelp() [][]key.Binding {
	listHelp := c.list.FullHelp()
	if len(listHelp) > 0 {
		// The list puts its cursor, page and jump keys in its first column. See list.Model.FullHelp.
		listHelp[0] = append(listHelp[0], c.picker.HalfPageUp, c.picker.HalfPageDown)
	}
	return append(c.picker.FullHelp(), listHelp...)
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
	case msg.attachment.Note != "":
		// What the attachment itself said, rather than a sentence built here from flags. It already
		// distinguishes a detach from a session that ended and from one that ended unexpectedly, and
		// saying it twice in two wordings is how the two drift apart.
		return msg.attachment.Note
	}
	// A child that printed nothing still came back, which is worth confirming: the list reappearing on
	// its own is otherwise indistinguishable from an attachment that never started.
	return "left " + msg.ref
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
