package tui

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
)

// TestALateAnswerForAnotherSessionIsDiscarded is the bug the pane is built around.
//
// A read is in flight while the cursor moves, so its answer arrives describing a session that is no
// longer selected. Painting it puts one session's output under another session's row, and unlike an
// empty pane that failure is invisible: the content is real, it is just an answer to a question nobody
// asked any more. The label makes it visible, the discard makes it impossible.
func TestALateAnswerForAnotherSessionIsDiscarded(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100), session("other", "b8l3n0y5", 200))
	h.sessions.output["@a7k2m9x4"] = "from work"
	h.sessions.output["@b8l3n0y5"] = "from other"
	h.list()

	// work's read is issued and held, the way a slow server would hold it.
	held := h.model.fetchPreview("a7k2m9x4", "@a7k2m9x4", 5)()

	h.pressCode(tea.KeyDown)
	if got, want := h.model.preview.want, "b8l3n0y5"; got != want {
		t.Fatalf("the pane is aimed at %q, want %q", got, want)
	}

	h.send(held)

	if got := h.model.preview.have; got != "" {
		t.Errorf("the pane accepted content from %q while %q is selected",
			got, h.model.preview.want)
	}
	if lines := h.model.preview.lines; len(lines) != 0 {
		t.Errorf("the pane shows %q, which belongs to the session the cursor left", lines)
	}
}

// TestMovingTheCursorClearsTheOldOutput is the same mistake from the other direction.
//
// Keeping the previous session's lines until the new ones arrive shows the wrong output under the new
// row for as long as the read takes, which on a busy server is exactly when a person is scrolling.
func TestMovingTheCursorClearsTheOldOutput(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100), session("other", "b8l3n0y5", 200))
	h.sessions.output["@a7k2m9x4"] = "from work"
	h.list()

	if got := h.model.preview.lines; !reflect.DeepEqual(got, []string{"from work"}) {
		t.Fatalf("the pane shows %q before the cursor moves, want work's output", got)
	}

	h.pressCode(tea.KeyDown)

	if got := h.model.preview.lines; len(got) != 0 {
		t.Errorf("after moving the cursor the pane still shows %q", got)
	}
	if got := h.model.preview.have; got != "" {
		t.Errorf("the pane claims to hold content from %q after moving the cursor", got)
	}
}

// TestThePaneReadsTheSelectedSessionByID checks which reference the read names.
//
// The same rule as attaching: a name is a binding that can be pointed elsewhere between the refresh
// that drew the row and this request, and reading the wrong session is quieter than attaching to it but
// no less wrong.
func TestThePaneReadsTheSelectedSessionByID(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100))
	h.list()

	if len(h.sessions.reads) != 1 {
		t.Fatalf("%d reads for one session with the pane open, want 1", len(h.sessions.reads))
	}
	if got, want := h.sessions.reads[0].Session, "@a7k2m9x4"; got != want {
		t.Errorf("read %q, want %q", got, want)
	}
	if h.sessions.reads[0].Unwrap {
		t.Error("the read unwrapped soft-wrapped lines, which hides everything past the pane's width")
	}
}

// TestNothingIsReadWhileThePaneIsClosed keeps the default picker as cheap as it was.
//
// The pane costs a request per refresh, so a picker that reads output nobody asked to see is paying for
// it on every poll. Also covers the toggle: closing the pane has to stop the reads, not just hide them.
func TestNothingIsReadWhileThePaneIsClosed(t *testing.T) {
	h := newHarness(t, session("work", "a7k2m9x4", 100), session("other", "b8l3n0y5", 200))
	h.list()
	h.pressCode(tea.KeyDown)
	h.list()

	if got := len(h.sessions.reads); got != 0 {
		t.Fatalf("%d reads with the pane closed, want none", got)
	}

	h.press("p")
	h.list()
	if got := len(h.sessions.reads); got == 0 {
		t.Fatal("opening the pane read nothing")
	}

	h.press("p")
	before := len(h.sessions.reads)
	h.list()
	h.pressCode(tea.KeyUp)
	if got := len(h.sessions.reads) - before; got != 0 {
		t.Errorf("%d reads after closing the pane again, want none", got)
	}
}

// TestARefreshRereadsTheSelectedSession covers the pane following a session that is still running.
//
// Without this the pane is a snapshot from whenever the cursor last moved, which for the session
// someone is watching is the one thing it must not be.
func TestARefreshRereadsTheSelectedSession(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100))
	h.sessions.output["@a7k2m9x4"] = "first"
	h.list()
	if got := h.model.preview.lines; !reflect.DeepEqual(got, []string{"first"}) {
		t.Fatalf("pane shows %q, want the first read", got)
	}

	h.sessions.output["@a7k2m9x4"] = "second"
	h.list()

	if got := h.model.preview.lines; !reflect.DeepEqual(got, []string{"second"}) {
		t.Errorf("pane shows %q after a refresh, want the newer output", got)
	}
	if got := len(h.sessions.reads); got != 2 {
		t.Errorf("%d reads across two refreshes, want 2", got)
	}
}

// TestAFilterMatchingNothingEmptiesThePane covers the state where there is no selection at all.
//
// The last session's output left on screen would be labelled with a row that is no longer in the list,
// which is the stale-content failure again, reached by typing rather than by scrolling.
func TestAFilterMatchingNothingEmptiesThePane(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100))
	h.sessions.output["@a7k2m9x4"] = "from work"
	h.list()

	h.run(h.press("/"))
	h.typeText("zzz")

	if _, ok := h.model.selected(); ok {
		t.Fatal("something is still selected, so this test proves nothing")
	}
	if got := h.model.preview.lines; len(got) != 0 {
		t.Errorf("the pane still shows %q with nothing selected", got)
	}
}

// TestAReadThatFailsSaysSo checks the pane reports rather than looking idle.
//
// An empty pane reads as "this session has printed nothing", which is a different fact from "its output
// could not be read", and the second one is usually a build without the terminal model.
func TestAReadThatFailsSaysSo(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100))
	h.sessions.readErr = errors.New("no terminal model for this session")
	h.list()

	if h.model.preview.err == nil {
		t.Fatal("a failed read reported nothing")
	}
	view := h.model.preview.view("work")
	if !strings.Contains(view, "cannot read this session") {
		t.Errorf("the pane renders %q, which does not say the read failed", view)
	}
}

// TestPreviewLinesRefusesToCarryEscapeSequences is the invariant that keeps session content from
// corrupting the picker's frame.
//
// Every byte here comes from a program inside a session and is about to be placed inside a frame
// bubbletea composes. cm's largest family of bugs is something writing to a terminal without knowing
// what state the stream is in, so the pane takes nothing on trust: the plain form of Read renders cells
// rather than replaying bytes and should already be clean, which is what the other instances of this bug
// also assumed.
func TestPreviewLinesRefusesToCarryEscapeSequences(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want []string
	}{
		{
			name: "an SGR sequence is removed rather than rendered",
			in:   "\x1b[31mred\x1b[0m plain",
			want: []string{"red plain"},
		},
		{
			name: "a cursor move cannot reposition the frame",
			in:   "before\x1b[2Jafter",
			want: []string{"beforeafter"},
		},
		{
			name: "a carriage return does not send the cursor back over the line",
			in:   "progress 50%\rprogress 90%",
			want: []string{"progress 50%progress 90%"},
		},
		{
			name: "a bell is not rung from inside a frame",
			in:   "done\x07",
			want: []string{"done"},
		},
		{
			name: "a NUL and a form feed are dropped",
			in:   "a\x00b\x0cc",
			want: []string{"abc"},
		},
		{
			name: "a tab becomes spaces, since the frame cannot see a tab stop",
			in:   "a\tb",
			want: []string{"a       b"},
		},
		{
			// internal/ansi withholds a sequence until it terminates and only gives up past 4 KiB, so one
			// cut off at the end of the data is dropped rather than emitted. That is the right end of the
			// trade here, for a reason the stripper's own callers do not have: this text goes into a frame,
			// and the only thing worse than losing two characters is putting a live escape into it.
			name: "a sequence cut off at the end is dropped, not emitted",
			in:   "text\x1b[",
			want: []string{"text"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := previewLines([]byte(tc.in), 40, 5)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("previewLines(%q) = %q, want %q", tc.in, got, tc.want)
			}
			for _, line := range got {
				for _, r := range line {
					if r == 0x1b {
						t.Fatalf("an escape byte survived into %q", line)
					}
				}
			}
		})
	}
}

// TestPreviewLinesShapesTheContentToThePane covers the trimming, the tail and the truncation together.
func TestPreviewLinesShapesTheContentToThePane(t *testing.T) {
	for _, tc := range []struct {
		name          string
		in            string
		width, height int
		want          []string
	}{
		{
			name:   "the last lines are kept, since that is where a session's news is",
			in:     "one\ntwo\nthree\nfour",
			width:  10,
			height: 2,
			want:   []string{"three", "four"},
		},
		{
			name:   "trailing blank lines are dropped before the tail is taken",
			in:     "one\ntwo\n\n\n\n",
			width:  10,
			height: 2,
			want:   []string{"one", "two"},
		},
		{
			name:   "a long line is truncated rather than wrapped onto a row the pane counted on",
			in:     "an extremely long line of output",
			width:  12,
			height: 2,
			want:   []string{"an extreme.."},
		},
		{
			name:   "blank lines between content are kept, since they are content",
			in:     "one\n\ntwo",
			width:  10,
			height: 5,
			want:   []string{"one", "", "two"},
		},
		{
			name:   "nothing in gives nothing out",
			in:     "",
			width:  10,
			height: 5,
			want:   []string{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := previewLines([]byte(tc.in), tc.width, tc.height)
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("previewLines(%q, %d, %d) = %q, want %q",
					tc.in, tc.width, tc.height, got, tc.want)
			}
		})
	}
}

// TestEverySessionIsVisibleWithThePaneOpen is a regression test for a layout order bug.
//
// The split depends on how many rows the list has, and the first layout happens on the window size,
// which arrives before any sessions do. Sizing the list for an empty list and never revisiting it left
// room for one row out of three, with a pagination indicator that made it look deliberate, while the
// pane below it looked perfect.
func TestEverySessionIsVisibleWithThePaneOpen(t *testing.T) {
	h := newHarnessPreviewing(t,
		session("quiet", "a7k2m9x4", 100),
		session("counter", "b8l3n0y5", 200),
		session("fancy", "c9m4o1z6", 300),
	)
	h.list()

	view := h.model.View().Content
	for _, name := range []string{"quiet", "counter", "fancy"} {
		if !strings.Contains(view, name) {
			t.Errorf("%q is not on screen, so the list was sized for fewer rows than it has", name)
		}
	}
	if got := h.model.list.Paginator.PerPage; got < 3 {
		t.Errorf("the list has room for %d rows, want at least the 3 sessions it holds", got)
	}
}

// TestThePaneGivesUpItsRoomOnAShortWindow checks the split, including where the pane disappears.
//
// A pane that keeps its share on a short window leaves a list of one row, which is a picker that cannot
// be used to pick. The list wins, because a picker without a list is not a picker.
func TestThePaneGivesUpItsRoomOnAShortWindow(t *testing.T) {
	for _, tc := range []struct {
		avail int
		want  int
	}{
		{avail: 0, want: 0},
		{avail: 10, want: 0},
		{avail: 11, want: 6},
		{avail: 20, want: 8},
		{avail: 40, want: 16},
	} {
		if got := previewHeightFor(tc.avail); got != tc.want {
			t.Errorf("previewHeightFor(%d) = %d, want %d", tc.avail, got, tc.want)
		}
		if got := previewHeightFor(tc.avail); got > 0 && tc.avail-got < listMinRows {
			t.Errorf("previewHeightFor(%d) = %d, which leaves the list %d rows, under the %d it keeps",
				tc.avail, got, tc.avail-got, listMinRows)
		}
	}
}

// TestThePaneIsExactlyItsHeight keeps the footer from moving as content arrives.
func TestThePaneIsExactlyItsHeight(t *testing.T) {
	p := preview{on: true, height: 6, width: 20, have: "a7k2m9x4", lines: []string{"one", "two"}}
	if got := lineCount(p.view("work")); got != 6 {
		t.Errorf("a pane of height 6 rendered %d lines", got)
	}

	p.lines = []string{"1", "2", "3", "4", "5", "6", "7", "8"}
	if got := lineCount(p.view("work")); got != 6 {
		t.Errorf("a pane of height 6 rendered %d lines when given more content than it has room for", got)
	}
}

// TestThePaneIsLabelledWithTheSessionItIsWaitingFor checks the header names the pane's own subject.
//
// Labelling it with the selection instead would name the right session while showing another's content
// during the moment a read is in flight, which is the confusion the discard exists to prevent.
func TestThePaneIsLabelledWithTheSessionItIsWaitingFor(t *testing.T) {
	h := newHarnessPreviewing(t, session("work", "a7k2m9x4", 100), session("other", "b8l3n0y5", 200))
	h.list()
	h.pressCode(tea.KeyDown)

	if got, want := h.model.previewLabel(), "other"; got != want {
		t.Errorf("the pane is labelled %q, want %q", got, want)
	}
}
