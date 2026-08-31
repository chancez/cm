package client

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
)

// newTestOverlay builds an overlay painting into a buffer, with the default keys.
func newTestOverlay(t *testing.T, rows, cols uint16) (*overlay, *bytes.Buffer) {
	t.Helper()
	detach, err := ParseDetachKey(DefaultDetachKey)
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	prefix, err := ParsePrefixKey(DefaultPrefixKey)
	if err != nil {
		t.Fatalf("ParsePrefixKey() error = %v", err)
	}
	var buf bytes.Buffer
	return &overlay{
		out:     &buf,
		size:    func() (uint16, uint16) { return rows, cols },
		enabled: true,
		prefix:  prefix,
		detach:  detach,
		session: "work",
		log:     slog.New(discardLogHandler{}),
	}, &buf
}

// sameResponse compares whole responses, treating a nil slice and an empty one as the same thing: a
// caller reads len(Send), and a test that distinguished them would fail on a difference nothing can see.
func sameResponse(a, b overlayResponse) bool {
	return string(a.Send) == string(b.Send) &&
		strings.Join(a.Run, "\x00") == strings.Join(b.Run, "\x00") &&
		a.Detach == b.Detach && a.Repaint == b.Repaint
}

func TestOverlayArmedActions(t *testing.T) {
	tests := []struct {
		name string
		// keys is what is typed after the prefix.
		keys     string
		want     overlayResponse
		wantMode overlayMode
	}{
		{
			name:     "d detaches",
			keys:     "d",
			want:     overlayResponse{Detach: true, Repaint: true},
			wantMode: overlayClosed,
		},
		{
			// The reason the overlay is not only a command line: cm eats its detach key, so this is the
			// only way ctrl-\ reaches a program and raises SIGQUIT.
			name:     "q sends the detach key to the program",
			keys:     "q",
			want:     overlayResponse{Send: []byte{0x1c}, Repaint: true},
			wantMode: overlayClosed,
		},
		{
			name:     "the prefix key twice sends the prefix key",
			keys:     "\x1d",
			want:     overlayResponse{Send: []byte{0x1d}, Repaint: true},
			wantMode: overlayClosed,
		},
		{
			name:     "the detach key while armed sends it too, rather than detaching",
			keys:     "\x1c",
			want:     overlayResponse{Send: []byte{0x1c}, Repaint: true},
			wantMode: overlayClosed,
		},
		{
			name:     "escape closes",
			keys:     "\x1b",
			want:     overlayResponse{Repaint: true},
			wantMode: overlayClosed,
		},
		{
			name:     "ctrl-c closes",
			keys:     "\x03",
			want:     overlayResponse{Repaint: true},
			wantMode: overlayClosed,
		},
		{
			name:     "colon opens the prompt",
			keys:     ":",
			wantMode: overlayPrompt,
		},
		{
			name:     "b opens a name field",
			keys:     "b",
			wantMode: overlayPrompt,
		},
		{
			name:     "an unbound key says so and waits to be dismissed",
			keys:     "z",
			wantMode: overlayResult,
		},
		{
			// One read holding the prefix and the action, which is what a fast typist produces.
			name:     "a command typed in one read runs",
			keys:     ":bind refactor\r",
			want:     overlayResponse{Run: []string{"bind", "refactor"}},
			wantMode: overlayRunning,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestOverlay(t, 24, 80)
			o.open()
			got := o.feed([]byte(tc.keys))
			if !sameResponse(got, tc.want) {
				t.Errorf("feed(%q) = %+v, want %+v", tc.keys, got, tc.want)
			}
			if o.mode != tc.wantMode {
				t.Errorf("feed(%q) left mode %v, want %v", tc.keys, o.mode, tc.wantMode)
			}
		})
	}
}

// The full cycle of a command: typed, dispatched, its output shown, dismissed, and a repaint asked for.
func TestOverlayRunsACommandAndShowsItsOutput(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()

	if got := o.feed([]byte(":bind ref")); !sameResponse(got, overlayResponse{}) {
		t.Fatalf("typing = %+v, want nothing to do yet", got)
	}
	// Backspace and ctrl-u edit the line, since a prompt that cannot be corrected is a prompt people
	// leave rather than use.
	o.feed([]byte("\x7f"))
	if got := string(o.line); got != "bind re" {
		t.Fatalf("after a backspace the line is %q, want %q", got, "bind re")
	}

	resp := o.feed([]byte("f\r"))
	if !sameResponse(resp, overlayResponse{Run: []string{"bind", "ref"}}) {
		t.Fatalf("enter = %+v, want a run of bind ref", resp)
	}
	if o.mode != overlayRunning {
		t.Fatalf("mode = %v after enter, want overlayRunning", o.mode)
	}

	// A keystroke while the command runs is dropped rather than acted on: it would otherwise answer the
	// result screen that has not appeared yet.
	if got := o.feed([]byte("d")); !sameResponse(got, overlayResponse{}) {
		t.Fatalf("a key while running = %+v, want nothing", got)
	}

	o.finish("ref now names @a7k2m9x4\n", nil)
	if o.mode != overlayResult {
		t.Fatalf("mode = %v after finish, want overlayResult", o.mode)
	}
	rows := o.rows(24, 80)
	if len(rows) != 2 || !strings.Contains(rows[0], "cm bind ref") ||
		rows[1] != "ref now names @a7k2m9x4" {
		t.Errorf("rows = %q, want the command on the bar and its output under it", rows)
	}

	// Any key dismisses, and is not acted on: the 'd' here must not detach.
	resp = o.feed([]byte("d"))
	if !sameResponse(resp, overlayResponse{Repaint: true}) {
		t.Errorf("dismissing = %+v, want only a repaint", resp)
	}
}

// A failing command shows its output and its error, since cm prints the useful part to stdout and the
// reason to stderr: showing one of them turned a refused kill into a silent no-op on screen.
func TestOverlayShowsAFailure(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte(":kill other\r"))
	o.finish("", errors.New("session other has a running command"))

	rows := o.rows(24, 80)
	if len(rows) != 2 || rows[1] != "error: session other has a running command" {
		t.Errorf("rows = %q, want the error under the bar", rows)
	}
}

// The invariant that matters most, and the one with six bugs behind it in this repo: while the overlay
// holds the keyboard, an answer the *program* is waiting for still reaches it.
//
// A program inside the session can have a query outstanding when the prefix is pressed. Its reply arrives
// in this same input stream, and an overlay that swallowed it would leave that program blocked on an
// answer that never comes.
func TestOverlayForwardsAnswersToTheProgram(t *testing.T) {
	answers := []string{
		"\x1b[24;80R",                    // cursor position report, the reply to CSI 6n
		"\x1b]11;rgb:2828/2c2c/34\x1b\\", // OSC 11 background colour
		"\x1b_Gi=1;OK\x1b\\",             // kitty graphics response
		"\x1b[I",                         // focus in
		"\x1b[<0;10;5M",                  // a mouse report
		"\x1b[?62;c",                     // a device attributes reply
	}
	for _, in := range answers {
		for _, mode := range []struct {
			name string
			keys string
		}{
			{name: "armed", keys: ""},
			{name: "prompt", keys: ":"},
		} {
			o, _ := newTestOverlay(t, 24, 80)
			o.open()
			o.feed([]byte(mode.keys))

			got := o.feed([]byte(in))
			if string(got.Send) != in {
				t.Errorf("%s: feed(%q) sent %q to the session, want it forwarded whole",
					mode.name, in, got.Send)
			}
			if !o.active() {
				t.Errorf("%s: feed(%q) closed the overlay, so the keystroke that opened it is wasted",
					mode.name, in)
			}
		}
	}
}

// A terminal reporting event types sends a release after every press, including the release of the prefix
// key itself. Treating one as a keypress closed the overlay the instant the key came up.
func TestOverlayIgnoresKeyReleasesAndRepeats(t *testing.T) {
	for _, in := range []string{"\x1b[93;5:3u", "\x1b[100;1:3u", "\x1b[100;1:2u"} {
		o, _ := newTestOverlay(t, 24, 80)
		o.open()
		got := o.feed([]byte(in))
		if !sameResponse(got, overlayResponse{}) {
			t.Errorf("feed(%q) = %+v, want it dropped", in, got)
		}
		if o.mode != overlayArmed {
			t.Errorf("feed(%q) left mode %v, want it still armed", in, o.mode)
		}
	}

	// And the press form of a plain key still works, which is how the overlay answers a keyboard the
	// program has put into report-all-keys mode.
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	if got := o.feed([]byte("\x1b[100u")); !sameResponse(got, overlayResponse{Detach: true, Repaint: true}) {
		t.Errorf("feed(kitty-encoded d) = %+v, want a detach", got)
	}
}

// Whatever follows the keystroke that closes the overlay was typed at the session, so it goes there
// rather than being dropped. A terminal delivers a fast sequence as one read, so this is the common case
// rather than a corner.
func TestOverlayForwardsWhatFollowsItsClose(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	got := o.feed([]byte("qls\r"))
	want := overlayResponse{Send: append([]byte{0x1c}, "ls\r"...), Repaint: true}
	if !sameResponse(got, want) {
		t.Errorf("feed(%q) = %+v, want %+v", "qls\\r", got, want)
	}
}

// A read-only follower's input is dropped by the server, so the overlay must not claim to have sent
// anything. It says so instead, and stays up to be read.
func TestOverlayReadOnlyCannotSend(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.readOnly = true
	o.open()
	got := o.feed([]byte("q"))
	if !sameResponse(got, overlayResponse{}) {
		t.Errorf("feed(q) on a read-only client = %+v, want nothing sent", got)
	}
	if o.mode != overlayResult || !strings.Contains(o.status, "read-only") {
		t.Errorf("mode %v status %q, want a read-only message", o.mode, o.status)
	}
}

// With no detach key configured there is nothing for q to send, and NUL is what a naive implementation
// would send: the zero KeySpec's Byte.
func TestOverlayQWithNoDetachKey(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	none, err := ParseDetachKey("none")
	if err != nil {
		t.Fatalf("ParseDetachKey() error = %v", err)
	}
	o.detach = none
	o.open()
	if got := o.feed([]byte("q")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("feed(q) with detaching disabled = %+v, want nothing sent", got)
	}
}

func TestOverlayRows(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()

	// One short of the width, always. Writing the last column leaves the terminal in pending wrap, and
	// one more byte then scrolls the session's screen out from under the model about to repaint it.
	for _, cols := range []int{80, 20, 2} {
		for _, line := range o.rows(24, cols) {
			if len(line) > cols-1 {
				t.Errorf("with %d columns a row is %d wide: %q", cols, len(line), line)
			}
		}
	}

	// The block never takes more than half the screen, and says what it cut rather than showing a
	// truncated list as if it were complete.
	o.status = "cm list"
	o.body = make([]string, 40)
	for i := range o.body {
		o.body[i] = fmt.Sprintf("session-%d", i)
	}
	rows := o.rows(10, 80)
	if len(rows) != 5 {
		t.Errorf("rows = %d with a 10-row terminal, want 5", len(rows))
	}
	if last := rows[len(rows)-1]; !strings.Contains(last, "more lines") {
		t.Errorf("last row = %q, want it to say what was cut", last)
	}

	// And nothing at all without a size, since a guessed row number writes into the middle of the
	// session.
	if got := o.rows(0, 0); got != nil {
		t.Errorf("rows(0, 0) = %q, want nothing painted", got)
	}
}

// What reaches the terminal has to be addressed absolutely, cleared, and bracketed by a cursor save and
// restore: the program in the session is drawing with the cursor cm is borrowing.
func TestOverlayPaintAndClose(t *testing.T) {
	o, buf := newTestOverlay(t, 24, 80)
	o.open()

	got := buf.String()
	for _, want := range []string{"\x1b7", "\x1b[24;1H", "\x1b[2K", "\x1b[7m", "\x1b8"} {
		if !strings.Contains(got, want) {
			t.Errorf("paint wrote %q, missing %q", got, want)
		}
	}
	if strings.Contains(got, "\n") || strings.Contains(got, "\r") {
		t.Errorf("paint wrote a newline, which scrolls the screen: %q", got)
	}

	// A taller block, then a shorter one: the rows the tall block used are blanked rather than left with
	// its top row on screen.
	o.status = "cm list"
	o.body = []string{"one", "two", "three"}
	o.paint()
	if o.painted != 4 {
		t.Fatalf("painted = %d, want 4", o.painted)
	}
	buf.Reset()
	o.body = nil
	o.paint()
	if o.painted != 1 {
		t.Errorf("painted = %d after shrinking, want 1", o.painted)
	}
	for _, row := range []string{"\x1b[21;1H\x1b[2K", "\x1b[22;1H\x1b[2K", "\x1b[23;1H\x1b[2K"} {
		if !strings.Contains(buf.String(), row) {
			t.Errorf("shrinking did not blank a row: want %q in %q", row, buf.String())
		}
	}

	// Closing erases what was painted and asks for a repaint, which is the caller's job: the rows held the
	// program's content and only cm's model knows what was there.
	buf.Reset()
	var resp overlayResponse
	o.close(&resp)
	if !resp.Repaint {
		t.Error("close did not ask for a repaint, so a blank row is left where the program's content was")
	}
	if !strings.Contains(buf.String(), "\x1b[24;1H\x1b[2K") {
		t.Errorf("close wrote %q, want the bottom row erased", buf.String())
	}
	if o.active() {
		t.Error("close left the overlay active")
	}

	// And a second close is not a second erase, so an overlay closed by a detach does not also repaint.
	buf.Reset()
	resp = overlayResponse{}
	o.close(&resp)
	if resp.Repaint || buf.Len() != 0 {
		t.Errorf("closing twice wrote %q and repaint=%v, want nothing", buf.String(), resp.Repaint)
	}
}

// Nothing is painted for a client that is not painting a terminal, which is a follower streaming to a
// pipe: an escape sequence there is corruption of whatever is consuming it.
func TestOverlayDisabledPaintsNothing(t *testing.T) {
	o, buf := newTestOverlay(t, 24, 80)
	o.enabled = false
	o.open()
	if buf.Len() != 0 {
		t.Errorf("painted %q with painting disabled", buf.String())
	}
}

func TestSplitCommandLine(t *testing.T) {
	tests := []struct {
		in      string
		want    []string
		wantErr bool
	}{
		{in: "bind refactor", want: []string{"bind", "refactor"}},
		{in: "  bind   refactor  ", want: []string{"bind", "refactor"}},
		{in: "", want: nil},
		// A leading cm is the habit, and rejecting it would be pedantic.
		{in: "cm bind refactor", want: []string{"bind", "refactor"}},
		{in: `tag note="fixing the parser"`, want: []string{"tag", "note=fixing the parser"}},
		{in: `tag note=''`, want: []string{"tag", "note="}},
		{in: `send --key ctrl-c`, want: []string{"send", "--key", "ctrl-c"}},
		{in: `bind "a b`, wantErr: true},
	}
	for _, tt := range tests {
		got, err := splitCommandLine(tt.in)
		if (err != nil) != tt.wantErr {
			t.Errorf("splitCommandLine(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			continue
		}
		if tt.wantErr {
			continue
		}
		if strings.Join(got, "\x00") != strings.Join(tt.want, "\x00") {
			t.Errorf("splitCommandLine(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

// Opening and then feeding nothing paints once, not twice.
//
// The caller opens the overlay and then feeds whatever followed the prefix key in the same read, which is
// usually nothing. Measured with the transcript hook against a real terminal: the whole block reached it
// twice for one keypress, which is a visible flicker over a program's screen and pointless traffic on
// every repaint.
func TestOverlayPaintsOncePerKeypress(t *testing.T) {
	o, buf := newTestOverlay(t, 24, 80)
	o.open()
	first := buf.Len()
	if first == 0 {
		t.Fatal("open painted nothing")
	}
	if got := o.feed(nil); !sameResponse(got, overlayResponse{}) {
		t.Errorf("feed(nil) = %+v, want nothing to do", got)
	}
	if buf.Len() != first {
		t.Errorf("feeding an empty read painted %d more bytes, want none", buf.Len()-first)
	}

	// A repaint after the session drew over the block must still write, identical content or not: that is
	// what makes the overlay heal instead of being left half erased.
	o.repaint()
	if buf.Len() <= first {
		t.Error("repaint wrote nothing, so an overlay painted over by the session would stay broken")
	}
}

// items builds a picker's worth of sessions for the cases below.
func pickItems(n int) []pickItem {
	out := make([]pickItem, 0, n)
	for i := range n {
		out = append(out, pickItem{
			Ref:    fmt.Sprintf("@id%d", i),
			Label:  fmt.Sprintf("session-%d", i),
			Detail: "~/dir",
		})
	}
	return out
}

// Choosing rather than typing, which is the whole point of the picker: s asks for the list, the list
// arrives, and enter switches to what is under the cursor.
func TestOverlayPickerSwitches(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()

	if got := o.feed([]byte("s")); !sameResponse(got, overlayResponse{List: true}) {
		t.Fatalf("feed(s) = %+v, want a request for the session list", got)
	}
	if o.mode != overlayPick {
		t.Fatalf("mode = %v, want overlayPick", o.mode)
	}
	// Something is on screen while the list is in flight, since a server round trip is not instant and a
	// blank bar reads as a broken keypress.
	if rows := o.rows(24, 80); len(rows) != 2 || !strings.Contains(rows[1], "listing sessions") {
		t.Errorf("rows while loading = %q, want a note that the list is coming", rows)
	}

	o.sessions(pickItems(3), nil)
	rows := o.rows(24, 80)
	if len(rows) != 4 || !strings.Contains(rows[0], "switch to") || !strings.Contains(rows[1], "> ") {
		t.Fatalf("rows = %q, want the prompt and a cursor on the first item", rows)
	}

	// Down one, then choose: the second session, by ID rather than by name.
	o.feed([]byte{0x0e})
	got := o.feed([]byte("\r"))
	if !sameResponse(got, overlayResponse{SwitchTo: "@id1", Repaint: true}) {
		t.Errorf("enter = %+v, want a switch to the second session", got)
	}
}

// Typing filters instead of moving, which is what lets one keystroke find a session among twenty. j and k
// cannot also mean movement for that reason, so the arrows and ctrl-n/ctrl-p do it.
func TestOverlayPickerFiltersAndMoves(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("s"))
	items := pickItems(12)
	items[7].Label = "notebook"
	o.sessions(items, nil)

	o.feed([]byte("note"))
	if got := len(o.pick.matches()); got != 1 {
		t.Errorf("matches after typing note = %d, want 1", got)
	}
	if got := o.feed([]byte("\r")); !sameResponse(got, overlayResponse{SwitchTo: "@id7", Repaint: true}) {
		t.Errorf("enter after filtering = %+v, want the filtered session", got)
	}

	// Backspace widens it again, and ctrl-u clears the filter outright.
	o, _ = newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("s"))
	o.sessions(items, nil)
	o.feed([]byte("note"))
	o.feed([]byte{0x7f})
	if got := len(o.pick.matches()); got != 1 {
		t.Errorf("matches after a backspace = %d, want 1 (not- still matches only notebook)", got)
	}
	o.feed([]byte{0x15})
	if got := len(o.pick.matches()); got != len(items) {
		t.Errorf("matches after ctrl-u = %d, want all %d", got, len(items))
	}

	// The window follows the cursor rather than letting it walk off the bottom.
	for range 11 {
		o.feed([]byte{0x0e})
	}
	body := o.pick.body(4)
	if len(body) != 4 || !strings.Contains(body[3], "> ") {
		t.Errorf("body with the cursor at the end = %q, want the cursor on the last visible row", body)
	}
}

// Killing takes one key of confirmation, because a mistyped filter plus enter would otherwise end
// someone's shell.
func TestOverlayPickerKillConfirms(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("k"))
	o.sessions(pickItems(2), nil)

	if got := o.feed([]byte("\r")); !sameResponse(got, overlayResponse{}) {
		t.Fatalf("choosing to kill = %+v, want nothing run yet", got)
	}
	if o.mode != overlayConfirm || !strings.Contains(o.bar(), "kill session-0?") {
		t.Fatalf("mode %v bar %q, want a confirmation naming the session", o.mode, o.bar())
	}
	// Any key but y abandons it, which is the safe answer being the easy one.
	if got := o.feed([]byte("x")); !sameResponse(got, overlayResponse{Repaint: true}) {
		t.Errorf("a key other than y = %+v, want the kill abandoned", got)
	}

	o, _ = newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("k"))
	o.sessions(pickItems(2), nil)
	o.feed([]byte("\r"))
	if got := o.feed([]byte("y")); !sameResponse(got, overlayResponse{Run: []string{"kill", "@id0"}}) {
		t.Errorf("y = %+v, want the kill run by ID", got)
	}
}

// Switching to the session you are already in is refused rather than performed: it is a no-op that costs a
// visible repaint and looks like a broken keypress.
func TestOverlayPickerRefusesTheCurrentSession(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("s"))
	items := pickItems(2)
	items[0].Current = true
	o.sessions(items, nil)

	if got := o.feed([]byte("\r")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("choosing the current session = %+v, want nothing done", got)
	}
	if !strings.Contains(o.status, "already attached") {
		t.Errorf("status = %q, want it to say so", o.status)
	}
}

// b asks for a name and nothing else, so the verb is the keypress rather than something to type. That was
// the friction in the first version: binding a name meant typing "bind" as well as the name.
func TestOverlayNamePrompt(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("b"))
	if o.mode != overlayPrompt || o.prompt != promptName {
		t.Fatalf("mode %v prompt %v, want a name field", o.mode, o.prompt)
	}
	if !strings.Contains(o.bar(), "name:") {
		t.Errorf("bar = %q, want it to ask for a name rather than look like a command line", o.bar())
	}
	if got := o.feed([]byte("refactor\r")); !sameResponse(got, overlayResponse{Run: []string{"bind", "refactor"}}) {
		t.Errorf("typing a name = %+v, want a bind", got)
	}
}

// Two things a real terminal showed: the terminal drew its own cursor on top of the bar, and the bar's
// highlight stopped where its text did, which reads as a stray line of output rather than as cm's.
func TestOverlayHidesTheCursorAndFillsTheWidth(t *testing.T) {
	o, buf := newTestOverlay(t, 24, 80)
	o.open()

	got := buf.String()
	if !strings.Contains(got, "\x1b[?25l") {
		t.Errorf("paint wrote %q, want the cursor hidden: the program's cursor is restored onto the bar", got)
	}
	// Padded on the way to the terminal rather than in rows(), which stays the logical view: what has to
	// span the pane is what was written.
	wantBar := overlayBarStyle + pad(o.rows(24, 80)[0], 79) + overlayStyleOff
	if !strings.Contains(got, wantBar) {
		t.Errorf("paint wrote %q, want a bar padded to 79 columns: %q", got, wantBar)
	}

	buf.Reset()
	var resp overlayResponse
	o.close(&resp)
	if !strings.Contains(buf.String(), "\x1b[?25h") {
		t.Errorf("close wrote %q, want the cursor shown again: nothing else will, since the restore blob "+
			"carries no cursor visibility", buf.String())
	}
}

// The block keeps its height as the filter narrows the list.
//
// Reported from real use: the picker is anchored to the bottom of the screen, so a list that shrinks as you
// type moves every row under your eyes and repaints the program's content between keystrokes.
func TestOverlayPickerHeightIsStableWhileFiltering(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("s"))
	items := pickItems(6)
	items[3].Label = "notebook"
	o.sessions(items, nil)

	want := len(o.rows(24, 80))
	if want != 7 {
		t.Fatalf("rows with 6 sessions = %d, want 7: a bar and six items", want)
	}
	for _, typed := range []string{"n", "o", "t", "e"} {
		o.feed([]byte(typed))
		if got := len(o.rows(24, 80)); got != want {
			t.Errorf("after typing %q the block is %d rows, want %d: the height must not move",
				typed, got, want)
		}
	}
	// Even with nothing matching at all, which is the extreme of the same problem.
	o.feed([]byte("zzz"))
	if got := len(o.rows(24, 80)); got != want {
		t.Errorf("with no matches the block is %d rows, want %d", got, want)
	}

	// A smaller terminal still shrinks it: the height is a preference, not a claim on space that is not
	// there.
	if got := len(o.rows(6, 80)); got > 3 {
		t.Errorf("rows on a 6-row terminal = %d, want at most 3", got)
	}
}

// fzf's movement keys, because that is the muscle memory this is measured against. ctrl-j is 0x0a, which
// makes Return CR alone: fzf does exactly this, and treating LF as enter would submit on every ctrl-j.
func TestOverlayPickerMovementKeys(t *testing.T) {
	down := []string{"\n", "\x0e", "\x1b[B", "\x1bOB", "\x1b[106;5u", "\x1b[110;5u"}
	up := []string{"\x0b", "\x10", "\x1b[A", "\x1bOA", "\x1b[107;5u", "\x1b[112;5u"}

	for _, keys := range down {
		o, _ := newTestOverlay(t, 24, 80)
		o.open()
		o.feed([]byte("s"))
		o.sessions(pickItems(3), nil)
		o.feed([]byte(keys))
		if got := o.pick.cursor; got != 1 {
			t.Errorf("%q moved the cursor to %d, want 1", keys, got)
		}
	}
	for _, keys := range up {
		o, _ := newTestOverlay(t, 24, 80)
		o.open()
		o.feed([]byte("s"))
		o.sessions(pickItems(3), nil)
		o.feed([]byte("\n\n"))
		o.feed([]byte(keys))
		if got := o.pick.cursor; got != 1 {
			t.Errorf("%q moved the cursor to %d, want 1", keys, got)
		}
	}

	// And Return still chooses, which is the half that would break if LF and CR were treated alike.
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("s"))
	o.sessions(pickItems(3), nil)
	if got := o.feed([]byte("\r")); !sameResponse(got, overlayResponse{SwitchTo: "@id0", Repaint: true}) {
		t.Errorf("Return = %+v, want it to choose", got)
	}
}

// A program that turned on the kitty protocol's report-all-keys makes the terminal send even ctrl-c as a
// CSI u sequence. Without the ctrl cases in decodeKittyKey, the overlay's own keys stop working under
// exactly the full-screen programs it exists for.
func TestOverlayCtrlKeysUnderTheKittyProtocol(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("s"))
	o.sessions(pickItems(3), nil)
	o.feed([]byte("session"))

	// ctrl-u clears the filter.
	o.feed([]byte("\x1b[117;5u"))
	if got := len(o.pick.filter); got != 0 {
		t.Errorf("filter after a kitty-encoded ctrl-u is %d runes, want 0", got)
	}
	// ctrl-c closes the overlay.
	if got := o.feed([]byte("\x1b[99;5u")); !sameResponse(got, overlayResponse{Repaint: true}) {
		t.Errorf("kitty-encoded ctrl-c = %+v, want the overlay closed", got)
	}
}

// Escape steps back one level rather than leaving the overlay, and ctrl-c leaves outright.
//
// Reported from use, about the help: reading what the keys are is not a reason to lose the overlay. The same
// applies to backing out of a list or a prompt, so escape means "up one level" everywhere and only the top
// level closes.
func TestOverlayEscapeStepsBack(t *testing.T) {
	cases := []struct {
		name string
		// enter is what opens the sub-screen.
		enter string
		// list reports whether the sub-screen needs the session list before escape is pressed.
		list bool
	}{
		{name: "help", enter: "?"},
		{name: "a switch list", enter: "s", list: true},
		{name: "a kill list", enter: "k", list: true},
		{name: "a name prompt", enter: "b"},
		{name: "a command line", enter: ":"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o, _ := newTestOverlay(t, 24, 80)
			o.open()
			o.feed([]byte(tc.enter))
			if tc.list {
				o.sessions(pickItems(3), nil)
			}

			// Escape: back to the action keys, still on screen, nothing asked of the caller.
			if got := o.feed([]byte("\x1b")); !sameResponse(got, overlayResponse{}) {
				t.Fatalf("escape = %+v, want nothing done: it steps back rather than closing", got)
			}
			if o.mode != overlayArmed || o.helping {
				t.Fatalf("mode %v helping %v, want the armed hints", o.mode, o.helping)
			}
			if !strings.Contains(o.bar(), "s switch") {
				t.Errorf("bar = %q, want the hints back", o.bar())
			}

			// And escape again leaves, since armed is the top level.
			if got := o.feed([]byte("\x1b")); !sameResponse(got, overlayResponse{Repaint: true}) {
				t.Errorf("a second escape = %+v, want the overlay closed", got)
			}
		})
	}
}

// ctrl-c leaves from anywhere, which is what keeps escape's extra level from being a trap.
func TestOverlayCtrlCLeavesFromAnywhere(t *testing.T) {
	for _, enter := range []string{"?", "s", "b", ":"} {
		o, _ := newTestOverlay(t, 24, 80)
		o.open()
		o.feed([]byte(enter))
		o.sessions(pickItems(2), nil)
		if got := o.feed([]byte{0x03}); !sameResponse(got, overlayResponse{Repaint: true}) {
			t.Errorf("ctrl-c after %q = %+v, want the overlay closed", enter, got)
		}
	}
}

// Every action key still works while the help is up, so pressing the key you have just read about does what
// it says rather than only dismissing the help.
func TestOverlayHelpKeysStillAct(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("?"))
	if !o.helping || len(o.rows(24, 80)) < 5 {
		t.Fatalf("help is not on screen: helping=%v rows=%d", o.helping, len(o.rows(24, 80)))
	}

	if got := o.feed([]byte("s")); !sameResponse(got, overlayResponse{List: true}) {
		t.Errorf("s from the help = %+v, want it to start the switch list", got)
	}
	if o.helping {
		t.Error("the help is still on screen after acting on one of its keys")
	}

	o, _ = newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("?"))
	if got := o.feed([]byte("d")); !sameResponse(got, overlayResponse{Detach: true, Repaint: true}) {
		t.Errorf("d from the help = %+v, want a detach", got)
	}
}

// ? toggles the help, as it does in `cm tui`: the key that opened it closes it. Escape also goes back, but
// a reader who opened the help with ? reaches for ? to put it away.
func TestOverlayHelpToggles(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()

	o.feed([]byte("?"))
	if !o.helping {
		t.Fatal("? did not open the help")
	}
	if got := o.feed([]byte("?")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("? again = %+v, want it to close the help without leaving the overlay", got)
	}
	if o.helping || o.mode != overlayArmed {
		t.Errorf("helping=%v mode=%v after a second ?, want the armed hints", o.helping, o.mode)
	}
	if !strings.Contains(o.bar(), "s switch") {
		t.Errorf("bar = %q, want the hints back", o.bar())
	}
	// And a third press opens it again, so it is a toggle rather than a one-way door.
	o.feed([]byte("?"))
	if !o.helping {
		t.Error("a third ? did not reopen the help")
	}
}

// The two shades, and the selection at the bar's brightness.
//
// Reverse video for both rather than colours, so each follows the terminal's own theme: a 256-colour grey
// that reads on a dark background is unreadable on a light one, and cm cannot know which it has. The rows
// under the bar add faint, and the chooser's selection drops it again, which is what makes the selected row
// findable now that every row has a background.
func TestOverlayShades(t *testing.T) {
	o, buf := newTestOverlay(t, 24, 80)
	o.open()
	if got := buf.String(); !strings.Contains(got, overlayBarStyle) {
		t.Errorf("the bar is not inverse: %q", got)
	}

	o.feed([]byte("s"))
	buf.Reset()
	o.sessions(pickItems(3), nil)

	got := buf.String()
	if !strings.Contains(got, overlayBodyStyle) {
		t.Errorf("the rows under the bar are not dimmed: %q", got)
	}
	// The selected row is bright, and it is the first item, so the bar's style appears twice: once for the
	// bar and once for the selection.
	if n := strings.Count(got, overlayBarStyle); n != 2 {
		t.Errorf("%d rows at the bar's brightness, want 2: the bar and the selection", n)
	}

	// Moving the cursor moves the bright row rather than adding one: the next paint still has exactly two,
	// and the second one is a row further down.
	buf.Reset()
	o.feed([]byte{0x0e})
	moved := buf.String()
	if n := strings.Count(moved, overlayBarStyle); n != 2 {
		t.Errorf("after moving, %d bright rows, want 2: the bar and the new selection", n)
	}
	if first, second := strings.Index(moved, "session-0"), strings.Index(moved, "session-1"); first > second {
		t.Fatalf("the rows are not in order, so the check below means nothing: %q", moved)
	}
	if strings.Index(moved, overlayBarStyle+"  session-1") < 0 &&
		!strings.Contains(moved, "> session-1") && !strings.Contains(moved, ">  session-1") {
		t.Errorf("the selection did not move to the second row: %q", moved)
	}

	// Nothing is highlighted outside a list, so a command's output is uniformly dimmed.
	o2, buf2 := newTestOverlay(t, 24, 80)
	o2.open()
	o2.feed([]byte(":list\r"))
	buf2.Reset()
	o2.finish("work\nother\n", nil)
	if n := strings.Count(buf2.String(), overlayBarStyle); n != 1 {
		t.Errorf("%d bright rows in a command's output, want 1: the bar alone", n)
	}
}
