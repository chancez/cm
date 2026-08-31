package tui

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// fakeSessions is a server that answers from a canned list and records what it was asked to change.
//
// Recording the requests rather than the calls, because what matters in almost every case here is the
// *reference* that was sent: a kill aimed at a name instead of an ID is the bug, and a fake that only
// counted calls would pass while doing it.
type fakeSessions struct {
	sessions []*serverv1.Session
	listErr  error

	listed  []*serverv1.ListRequest
	killed  []*serverv1.KillRequest
	bound   []*serverv1.BindRequest
	unbound []*serverv1.UnbindRequest

	killErr      error
	killResponse *serverv1.KillResponse
	bindErr      error

	// output is what Read answers, keyed by the session reference asked for.
	output  map[string]string
	reads   []*serverv1.ReadRequest
	readErr error
}

func (f *fakeSessions) List(
	_ context.Context, req *serverv1.ListRequest,
) (*serverv1.ListResponse, error) {
	f.listed = append(f.listed, req)
	if f.listErr != nil {
		return nil, f.listErr
	}
	return &serverv1.ListResponse{Sessions: f.sessions}, nil
}

func (f *fakeSessions) Kill(
	_ context.Context, req *serverv1.KillRequest,
) (*serverv1.KillResponse, error) {
	f.killed = append(f.killed, req)
	if f.killErr != nil {
		return nil, f.killErr
	}
	if f.killResponse != nil {
		return f.killResponse, nil
	}
	return &serverv1.KillResponse{Killed: req.Sessions}, nil
}

func (f *fakeSessions) Bind(
	_ context.Context, req *serverv1.BindRequest,
) (*serverv1.BindResponse, error) {
	f.bound = append(f.bound, req)
	if f.bindErr != nil {
		return nil, f.bindErr
	}
	return &serverv1.BindResponse{}, nil
}

func (f *fakeSessions) Unbind(
	_ context.Context, req *serverv1.UnbindRequest,
) (*serverv1.UnbindResponse, error) {
	f.unbound = append(f.unbound, req)
	return &serverv1.UnbindResponse{}, nil
}

func (f *fakeSessions) Read(
	_ context.Context, req *serverv1.ReadRequest,
) (*serverv1.ReadResponse, error) {
	f.reads = append(f.reads, req)
	if f.readErr != nil {
		return nil, f.readErr
	}
	return &serverv1.ReadResponse{Data: []byte(f.output[req.Session])}, nil
}

// harness drives a model the way bubbletea would, without a terminal.
//
// Commands are run only when a test asks for it. Running every returned command automatically would
// sleep for a refresh interval on each one, since the poll is a tea.Tick, and would turn a unit test
// into a timing test.
type harness struct {
	t        *testing.T
	model    model
	sessions *fakeSessions
	// attached records every reference the attach function was given, in order.
	attached []string
	// result is what the attach function reports back.
	result Attachment
	// switched records every reference the switch function was given, and switchErr is what it reports.
	switched  []string
	switchErr error
}

func newHarness(t *testing.T, sessions ...*serverv1.Session) *harness {
	return newHarnessWith(t, Options{}, sessions...)
}

// newHarnessSwitching is newHarness with a caller that can be switched, which is what the picker has when
// the overlay opened it from inside a session.
func newHarnessSwitching(t *testing.T, sessions ...*serverv1.Session) *harness {
	t.Helper()
	h := &harness{t: t}
	opts := Options{Switch: func(ref string) error {
		h.switched = append(h.switched, ref)
		return h.switchErr
	}}
	return newHarnessWithInto(t, h, opts, sessions...)
}

// newHarnessPreviewing is newHarness with the output pane open.
func newHarnessPreviewing(t *testing.T, sessions ...*serverv1.Session) *harness {
	return newHarnessWith(t, Options{Preview: true}, sessions...)
}

func newHarnessWith(t *testing.T, opts Options, sessions ...*serverv1.Session) *harness {
	t.Helper()
	return newHarnessWithInto(t, &harness{t: t}, opts, sessions...)
}

// newHarnessWithInto builds into a harness the caller has already started filling, so a case can close over
// it from an Options callback.
func newHarnessWithInto(
	t *testing.T, h *harness, opts Options, sessions ...*serverv1.Session,
) *harness {
	t.Helper()
	h.sessions = &fakeSessions{sessions: sessions, output: map[string]string{}}
	// No polling. A timer in a unit test is either a second of waiting or a race, and every refresh
	// these tests care about is one they ask for. See Options.Refresh.
	opts.Refresh = new(time.Duration)
	opts.Sessions = h.sessions
	opts.Attach = func(_ context.Context, ref string) (Attachment, error) {
		h.attached = append(h.attached, ref)
		return h.result, nil
	}
	h.model = newModel(context.Background(), opts)
	// A size first, as bubbletea sends one before anything else. Without it the list has no height and
	// no rows are visible, which makes every selection assertion below vacuous.
	h.send(tea.WindowSizeMsg{Width: 80, Height: 24})
	return h
}

// send applies a message and keeps the resulting model.
func (h *harness) send(msg tea.Msg) tea.Cmd {
	h.t.Helper()
	next, cmd := h.model.Update(msg)
	h.model = next.(model)
	return cmd
}

// press applies a printable key.
func (h *harness) press(k string) tea.Cmd {
	h.t.Helper()
	return h.send(tea.KeyPressMsg{Code: rune(k[0]), Text: k})
}

// pressCode applies a named key, like enter or esc, which has a code rather than text.
func (h *harness) pressCode(code rune) tea.Cmd {
	h.t.Helper()
	return h.send(tea.KeyPressMsg{Code: code})
}

// pressCtrl applies a control key, like ctrl+d, which arrives as a rune plus a modifier.
func (h *harness) pressCtrl(k rune) tea.Cmd {
	h.t.Helper()
	return h.send(tea.KeyPressMsg{Code: k, Mod: tea.ModCtrl})
}

// maxRunSteps bounds one call to run, so a message loop fails the test instead of hanging it.
//
// Learned the hard way: chasing the cursor's blink through this runner ran until the five minute tool
// timeout, and a hang says nothing about which loop it was. Any real chain here is a handful of
// messages deep.
const maxRunSteps = 50

// run executes a command and feeds its message back in, which is what bubbletea's loop does.
//
// Batches are unwrapped and run in order rather than handed to the model, since a BatchMsg is the
// runtime's business and the model would pass it to the list as an unknown message. This is safe only
// because these harnesses do not poll: the refresh timer is the one command that blocks.
func (h *harness) run(cmd tea.Cmd) {
	h.t.Helper()
	h.runStep(cmd, 0)
}

func (h *harness) runStep(cmd tea.Cmd, depth int) {
	h.t.Helper()
	if depth > maxRunSteps {
		h.t.Fatalf("commands are still producing messages %d deep, which is a loop", depth)
	}
	if cmd == nil {
		return
	}
	msg := cmd()
	if batch, ok := msg.(tea.BatchMsg); ok {
		for _, c := range batch {
			h.runStep(c, depth+1)
		}
		return
	}
	if msg == nil {
		return
	}
	// The cursor's blink is a timer that reschedules itself forever, so it is dropped rather than
	// chased. Matched on the package its type comes from because the first message in that chain is
	// unexported, so there is nothing to assert against. Nothing in the picker depends on a blink.
	if t := reflect.TypeOf(msg); t != nil && strings.HasPrefix(t.String(), "cursor.") {
		return
	}
	// A message can produce another command, and the runtime runs those too: a list arriving is what
	// leads to the pane being read, so a harness that stopped at the first message would test a picker
	// whose preview never loads.
	h.runStep(h.send(msg), depth+1)
}

// typeText presses each key and runs what it produced, which the list's filter needs: filtering is
// applied by a command rather than inside Update, so a filter typed without running them matches
// everything.
func (h *harness) typeText(text string) {
	h.t.Helper()
	for _, r := range text {
		h.run(h.press(string(r)))
	}
}

// list answers with whatever the fake holds now, and runs whatever that led to, which is how a
// refresh also reaches the preview pane.
func (h *harness) list() {
	h.t.Helper()
	h.run(h.model.fetch())
}

// selectedID is the ID of the session under the cursor, or "" when there is none.
func (h *harness) selectedID() string {
	h.t.Helper()
	it, ok := h.model.selected()
	if !ok {
		return ""
	}
	return it.session.Id
}

// session builds a running session with a name and an ID.
func session(name, id string, createdAt int64) *serverv1.Session {
	return &serverv1.Session{
		Name:          name,
		Id:            id,
		Names:         []string{name},
		State:         serverv1.SessionState_SESSION_STATE_RUNNING,
		CreatedAtUnix: createdAt,
	}
}

// TestAttachingUsesTheIDAndNotTheName is the assertion the whole picker rests on.
//
// A name is a binding: `cm bind` and `cm switch` can point it at another session while the picker is
// showing it, and the row was drawn from a list that is up to a second old. Attaching by the name in
// that row would then land somewhere the user did not choose, and worse, `cm attach` creates a session
// for a name that holds nothing, so a name that has since been unbound would silently make a new
// shell. An ID cannot do either: internal/server.Manager.Open refuses a stale one rather than creating
// for it.
func TestAttachingUsesTheIDAndNotTheName(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100), session("other", "b8l3n0y5", 200))
	h.list()

	h.pressCode(tea.KeyDown)
	h.pressCode(tea.KeyEnter)

	if h.model.handoff == nil {
		t.Fatal("enter on a session did not hand the terminal to an attachment")
	}
	if got, want := h.model.handoff.ref, "@b8l3n0y5"; got != want {
		t.Errorf("attached to %q, want %q: the row's ID reference, not its name", got, want)
	}
}

// TestNewSessionAttachesWithNoReference checks that "n" lets the server name the session.
func TestNewSessionAttachesWithNoReference(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	h.press("n")

	if h.model.handoff == nil {
		t.Fatal("n did not hand the terminal to an attachment")
	}
	if got := h.model.handoff.ref; got != "" {
		t.Errorf("new session asked for %q, want an empty reference so the server allocates one", got)
	}
}

// TestDetachingReportsAndRefreshes covers the loop the picker exists for: the attachment ends, the
// list says what happened, and the rows are re-read rather than waiting out the poll interval.
func TestDetachingReportsAndRefreshes(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()
	before := len(h.sessions.listed)

	h.run(h.send(attachedMsg{
		ref:        "@a7k2m9x4",
		attachment: Attachment{Note: "detached from work"},
	}))

	if got, want := h.model.status, "detached from work"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}
	if got := len(h.sessions.listed) - before; got != 1 {
		t.Errorf("%d refreshes after detaching, want exactly 1", got)
	}
}

// TestAnAttachmentThatSaidNothingIsStillConfirmed covers a child that printed nothing at all.
//
// The list reappearing on its own is otherwise indistinguishable from an attachment that never started,
// which is the case a stale reference produces, so silence needs a line of its own.
func TestAnAttachmentThatSaidNothingIsStillConfirmed(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	h.send(attachedMsg{ref: "@a7k2m9x4", attachment: Attachment{}})

	if got, want := h.model.status, "left @a7k2m9x4"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}
}

// TestFilteringDoesNotRunActions is a trap this design walks straight into.
//
// The picker's actions are single printable letters, and the list's filter is a text field. If the
// bindings are checked before the filter state, typing a session name runs commands: "n" creates a
// session, "x" offers to kill one, and "q" quits mid-word. Nothing about that is visible in a passing
// build, because the letters do reach the filter as well.
func TestFilteringDoesNotRunActions(t *testing.T) {
	h := newHarness(t, session("nginx", "a7k2m9x4", 100))
	h.list()

	// "/" starts the filter, then a word made entirely of action keys.
	h.press("/")
	for _, k := range []string{"n", "x", "r", "q"} {
		h.press(k)
	}

	if h.model.handoff != nil {
		t.Error("typing in the filter created a session")
	}
	if h.model.mode != modeList {
		t.Errorf("typing in the filter left the picker in mode %d, want the list", h.model.mode)
	}
	if got := len(h.sessions.killed); got != 0 {
		t.Errorf("typing in the filter sent %d kills", got)
	}
	if got, want := h.model.list.FilterInput.Value(), "nxrq"; got != want {
		t.Errorf("filter reads %q, want %q: the keys must reach the filter and nothing else", got, want)
	}
}

// TestKillAsksFirst checks that a keystroke away from ending someone's shell is a question.
func TestKillAsksFirst(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	h.press("x")
	if h.model.mode != modeConfirmKill {
		t.Fatalf("x left the picker in mode %d, want the kill confirmation", h.model.mode)
	}
	if got := len(h.sessions.killed); got != 0 {
		t.Fatalf("x killed %d sessions before being answered", got)
	}

	h.press("n")
	if got := len(h.sessions.killed); got != 0 {
		t.Errorf("answering n killed %d sessions", got)
	}
	if h.model.mode != modeList {
		t.Errorf("answering n left the picker in mode %d, want the list", h.model.mode)
	}
}

// TestARefreshBetweenTheQuestionAndTheAnswerKillsWhatWasAsked is the bug the captured target exists
// to prevent.
//
// The list refreshes on a timer while the confirmation is on screen, and the case that moves the cursor
// is the selected session ending on its own: there is then nothing to put the cursor back on, so it
// stays where it is and a different session is under it. A kill that re-read the selection at "y" would
// end that one, which is a shell nobody asked to close and a confirmation on screen that named another.
//
// Capturing sends the kill to the session that was named. That reference is stale by now, and the
// server refuses a stale ID rather than creating for it, so the outcome is an error about the session
// the user meant instead of damage to one they did not.
func TestARefreshBetweenTheQuestionAndTheAnswerKillsWhatWasAsked(t *testing.T) {
	doomed := session("doomed", "b8l3n0y5", 200)
	bystander := session("bystander", "c9m4o1z6", 300)
	h := newHarness(t, doomed, bystander)
	h.list()

	if got, want := h.selectedID(), "b8l3n0y5"; got != want {
		t.Fatalf("selected %q before asking, want %q", got, want)
	}
	h.press("x")

	// The session the question named ends by itself, so the row under the cursor is now the bystander.
	h.sessions.sessions = []*serverv1.Session{bystander}
	h.list()
	if got, want := h.selectedID(), "c9m4o1z6"; got != want {
		t.Fatalf("the cursor is on %q, want %q: this test proves nothing unless it moved", got, want)
	}

	h.run(h.press("y"))

	if len(h.sessions.killed) != 1 {
		t.Fatalf("%d kills sent, want 1", len(h.sessions.killed))
	}
	want := &serverv1.KillRequest{Sessions: []string{"@b8l3n0y5"}}
	if got := h.sessions.killed[0]; !reflect.DeepEqual(got, want) {
		t.Errorf("killed %v, want %v: the session the question named", got, want)
	}
}

// TestTheCursorStaysOnItsSessionAcrossARefresh checks the property that makes a polled list usable.
//
// Restoring the cursor by row index instead would move it whenever anything older than the selected
// session ended, which under a one second poll means the selection walks away while it is being aimed
// at.
func TestTheCursorStaysOnItsSessionAcrossARefresh(t *testing.T) {
	first := session("first", "a7k2m9x4", 100)
	second := session("second", "b8l3n0y5", 200)
	third := session("third", "c9m4o1z6", 300)
	h := newHarness(t, first, second, third)
	h.list()

	h.pressCode(tea.KeyDown)
	h.pressCode(tea.KeyDown)
	if got, want := h.selectedID(), "c9m4o1z6"; got != want {
		t.Fatalf("selected %q, want %q", got, want)
	}

	h.sessions.sessions = []*serverv1.Session{second, third}
	h.list()

	if got, want := h.selectedID(), "c9m4o1z6"; got != want {
		t.Errorf("after a refresh the cursor is on %q, want %q", got, want)
	}
}

// manySessions builds n running sessions, in the order the server would list them.
//
// Enough of them to fill more than one page, which is what the half page keys are for and what a
// three session fixture cannot show: on a list that fits, every jump lands on the last row and a test
// over it passes whatever the step size is.
func manySessions(n int) []*serverv1.Session {
	sessions := make([]*serverv1.Session, 0, n)
	for i := range n {
		sessions = append(sessions, session(fmt.Sprintf("s%02d", i), fmt.Sprintf("a7k2m9%02d", i), int64(100+i)))
	}
	return sessions
}

// TestHalfPageMovesHalfTheRowsAndTurnsThePage covers ctrl-d and ctrl-u.
//
// Half of the list's own PerPage rather than half the window: the pane and the expanded help both take
// rows off the list, so a step measured from the height jumps further than there is list to jump
// through. The page turn is asserted as well, because the list is paginated rather than scrolled, and a
// step that moved the cursor without turning the page would stop at the bottom of the first page while
// the cursor kept counting.
func TestHalfPageMovesHalfTheRowsAndTurnsThePage(t *testing.T) {
	h := newHarness(t, manySessions(40)...)
	h.list()

	perPage := h.model.list.Paginator.PerPage
	half := perPage / 2
	if half < 2 {
		t.Fatalf("PerPage is %d, so a half page is %d: this test proves nothing that small", perPage, half)
	}

	h.pressCtrl('d')
	if got, want := h.model.list.Index(), half; got != want {
		t.Fatalf("ctrl-d put the cursor on row %d, want %d, half of a %d row page", got, want, perPage)
	}
	if got := h.model.list.Paginator.Page; got != 0 {
		t.Errorf("one ctrl-d turned to page %d, want to still be on the first page", got)
	}

	// A second and a third, which is where the page has to turn: two half pages is a page.
	h.pressCtrl('d')
	h.pressCtrl('d')
	if got, want := h.model.list.Index(), 3*half; got != want {
		t.Errorf("three ctrl-d put the cursor on row %d, want %d", got, want)
	}
	if got, want := h.model.list.Paginator.Page, 3*half/perPage; got != want {
		t.Errorf("three ctrl-d left the list on page %d, want %d", got, want)
	}
	if got, want := h.selectedID(), fmt.Sprintf("a7k2m9%02d", 3*half); got != want {
		t.Errorf("selected %q, want %q", got, want)
	}

	h.pressCtrl('u')
	if got, want := h.model.list.Index(), 2*half; got != want {
		t.Errorf("ctrl-u put the cursor on row %d, want %d", got, want)
	}
}

// TestTheExpandedHelpNamesTheHalfPageKeysAndStillFits covers where the keys are described.
//
// Both halves matter. A key that moves the cursor and is in no help line is one nobody finds, and the
// expanded help is not truncated to the window, so a column too many silently pushes the last columns
// off the right edge. Measured in a real 100 column terminal: the help reached column 89 before these
// keys existed, a column of their own took it to 114 and cut the filter and quit columns off, and
// putting them in the list's navigation column leaves the width where it was.
func TestTheExpandedHelpNamesTheHalfPageKeysAndStillFits(t *testing.T) {
	h := newHarness(t, manySessions(40)...)
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.list()
	h.press("?")

	view := h.model.help.View(h.model.fullHelp())
	for _, want := range []string{"ctrl+u", "half page up", "ctrl+d", "half page down"} {
		if !strings.Contains(view, want) {
			t.Errorf("the expanded help does not mention %q:\n%s", want, view)
		}
	}
	if got := lipgloss.Width(view); got > h.model.width {
		t.Errorf("the expanded help is %d columns wide in a %d column window, so its last column is cut:\n%s",
			got, h.model.width, view)
	}
}

// TestHalfPageStopsAtTheEnds checks that neither key wraps.
//
// A picker that wrapped from the last session to the first would move the selection a long way on a key
// pressed to move it a little, and the next keystroke acts on whatever is under it: enter attaches to
// it and x offers to kill it.
func TestHalfPageStopsAtTheEnds(t *testing.T) {
	sessions := manySessions(40)
	h := newHarness(t, sessions...)
	h.list()

	h.pressCtrl('u')
	if got := h.model.list.Index(); got != 0 {
		t.Errorf("ctrl-u on the first row moved to row %d, want to stay on 0", got)
	}

	// Far more presses than there are pages, so the end is reached whatever the window size works out to.
	for range len(sessions) {
		h.pressCtrl('d')
	}
	if got, want := h.selectedID(), sessions[len(sessions)-1].Id; got != want {
		t.Errorf("ctrl-d past the end selected %q, want %q, the last session", got, want)
	}
}

// TestHalfPageWhileFilteringBelongsToTheFilter is the same trap TestFilteringDoesNotRunActions covers,
// arrived at from the other side.
//
// ctrl-u is delete-to-start in a text field, so a filter being typed has its own use for it. The keys
// are checked after the filter state for that reason, and a picker that checked them first would move
// the cursor under a filter the user is still typing.
func TestHalfPageWhileFilteringBelongsToTheFilter(t *testing.T) {
	h := newHarness(t, manySessions(40)...)
	h.list()

	h.press("/")
	h.typeText("s1")
	before := h.model.list.Index()

	h.pressCtrl('u')

	if got := h.model.list.Index(); got != before {
		t.Errorf("ctrl-u while filtering moved the cursor to row %d, want it left on %d", got, before)
	}
	if got := h.model.list.FilterInput.Value(); got != "" {
		t.Errorf("filter reads %q after ctrl-u, want it cleared: the key belongs to the field", got)
	}
}

// TestRenameBindsTheNewNameAndDropsTheOld covers a rename being two requests, in order.
func TestRenameBindsTheNewNameAndDropsTheOld(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	h.press("r")
	if h.model.mode != modeRename {
		t.Fatalf("r left the picker in mode %d, want the rename prompt", h.model.mode)
	}
	for _, k := range []string{"c", "m"} {
		h.press(k)
	}
	h.run(h.pressCode(tea.KeyEnter))

	wantBind := []*serverv1.BindRequest{{Name: "cm", Session: "@a7k2m9x4"}}
	if got := h.sessions.bound; !reflect.DeepEqual(got, wantBind) {
		t.Errorf("bound %v, want %v", got, wantBind)
	}
	wantUnbind := []*serverv1.UnbindRequest{{Name: "work"}}
	if got := h.sessions.unbound; !reflect.DeepEqual(got, wantUnbind) {
		t.Errorf("unbound %v, want %v", got, wantUnbind)
	}
}

// TestRenameKeepsEveryNameOfASessionThatHasSeveral checks that a rename does not guess.
//
// Several names are deliberate: `cm rebind` marks a borrowed name, so a window can name a session
// that something else also names. Dropping one of those would take a name out from under whatever
// bound it.
func TestRenameKeepsEveryNameOfASessionThatHasSeveral(t *testing.T) {
	s := session("work", "a7k2m9x4", 100)
	s.Names = []string{"work", "borrowed"}
	h := newHarness(t, s)
	h.list()

	h.press("r")
	h.press("c")
	h.run(h.pressCode(tea.KeyEnter))

	if got := len(h.sessions.bound); got != 1 {
		t.Errorf("%d binds, want 1", got)
	}
	if got := len(h.sessions.unbound); got != 0 {
		t.Errorf("%d unbinds, want none: a session with two names has no single name to replace", got)
	}
}

// TestRenameRefusesANameThatCouldBeAReference checks the validation happens before a request.
//
// The sigil is what makes a name and an ID impossible to confuse, so a name containing one is refused
// everywhere in cm. Refusing it here as well costs nothing and keeps the message the same.
func TestRenameRefusesANameThatCouldBeAReference(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	h.press("r")
	h.press("@")
	h.press("x")
	h.run(h.pressCode(tea.KeyEnter))

	if got := len(h.sessions.bound); got != 0 {
		t.Errorf("%d binds sent for a name containing the ID sigil, want none", got)
	}
	if h.model.err == nil {
		t.Error("a rejected name reported nothing")
	}
}

// TestAFailedListKeepsTheRowsAndSaysSo checks that one bad poll does not blank the list.
//
// A server that has gone away is usually coming back, and the rows are what the user is reading. The
// error is what says they cannot be trusted.
func TestAFailedListKeepsTheRowsAndSaysSo(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	h.sessions.listErr = errors.New("connection reset")
	h.list()

	if got, want := len(h.model.list.Items()), 1; got != want {
		t.Errorf("%d rows after a failed refresh, want %d", got, want)
	}
	if h.model.listErr == nil {
		t.Error("a failed refresh reported nothing")
	}
}

// TestAKillThatFailsPerSessionIsNotReportedAsSuccess covers the shape of KillResponse.
//
// The call succeeds while naming a session it could not end, so reading only the transport error would
// print "killed work" for a shell that is still running.
func TestAKillThatFailsPerSessionIsNotReportedAsSuccess(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()
	h.sessions.killResponse = &serverv1.KillResponse{
		Errors: map[string]string{"@a7k2m9x4": "still has a client attached"},
	}

	h.press("x")
	h.run(h.press("y"))

	if h.model.err == nil {
		t.Fatal("a kill that failed for the session reported nothing")
	}
	if h.model.status == "killed work" {
		t.Error("a failed kill was reported as a kill")
	}
}

// The switch binding moves the caller, by ID, and ends the picker.
//
// By ID for the reason the kill does: the list refreshes on a timer, so a name can be pointing at another
// session between the row being drawn and the key being pressed, and a switch aimed at a name would send
// the window somewhere the user did not choose.
func TestSwitchMovesTheCallerByID(t *testing.T) {
	h := newHarnessSwitching(t, session("work", "a7k2m9x4", 100), session("other", "b8l3n0y5", 200))
	h.list()

	cmd := h.press("s")
	if cmd == nil {
		t.Fatal("s did nothing, want a switch")
	}
	msg := cmd()
	if got, want := h.switched, []string{"@a7k2m9x4"}; !reflect.DeepEqual(got, want) {
		t.Errorf("switched to %v, want %v", got, want)
	}

	// A successful switch quits: the caller moves the window as this process exits, so there is nothing
	// left for the picker to show.
	if _, quit := h.send(msg)().(tea.QuitMsg); !quit {
		t.Errorf("a completed switch did not quit the picker")
	}
}

// Without a caller to move there is no switch key at all: not inert, absent. That is the picker's own
// case, a window with no session yet, where switching has no meaning.
func TestSwitchIsAbsentWithoutACaller(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100))
	h.list()

	if h.model.keys.Switch.Enabled() {
		t.Error("the switch binding is enabled with no way to switch")
	}
	if cmd := h.press("s"); cmd != nil {
		t.Errorf("s produced %T, want nothing: the binding is disabled", cmd())
	}
	if len(h.switched) != 0 {
		t.Errorf("switched %v with no caller to move", h.switched)
	}

	// And it is not offered in the help, since bubbles leaves a disabled binding out.
	h.press("?")
	if got := h.model.help.View(h.model.fullHelp()); strings.Contains(got, "switch here") {
		t.Errorf("the help offers the switch key with no caller to move:\n%s", got)
	}
}

// A failed switch says so and leaves the picker up, rather than quitting into a window that did not move.
func TestSwitchReportsAFailure(t *testing.T) {
	h := newHarnessSwitching(t, session("work", "a7k2m9x4", 100))
	h.switchErr = errors.New("no client to move")
	h.list()

	h.run(h.press("s"))
	if h.model.err == nil {
		t.Fatal("a failed switch reported nothing")
	}
	if got := h.model.footer(); !strings.Contains(got, "no client to move") {
		t.Errorf("the failure is not in the footer:\n%s", got)
	}
}

// With a caller to move, the expanded help offers the key and still fits the window. The picker's help is
// not truncated, and the layout measures the footer by counting newlines, so a column that overflows
// silently loses the columns to its right.
func TestTheExpandedHelpFitsWithTheSwitchKey(t *testing.T) {
	h := newHarnessSwitching(t, manySessions(40)...)
	h.send(tea.WindowSizeMsg{Width: 100, Height: 30})
	h.list()
	h.press("?")

	got := h.model.help.View(h.model.fullHelp())
	if !strings.Contains(got, "switch here") {
		t.Fatalf("the expanded help does not name the switch key:\n%s", got)
	}
	if w := lipgloss.Width(got); w > h.model.width {
		t.Errorf("the expanded help is %d columns wide in a %d column window, so its last column is cut:\n%s",
			w, h.model.width, got)
	}
	// The columns to the right of the new key are what an overflow eats first.
	for _, want := range []string{"filter", "quit"} {
		if !strings.Contains(got, want) {
			t.Errorf("the expanded help lost the %q column:\n%s", want, got)
		}
	}
}
