package client

import (
	"fmt"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/vt"
)

// A click presses the button drawn under it, wherever the layout put it, and a click beside a button
// presses nothing.
//
// Found on the screen the paint produces rather than from the notice's own record of where it drew, so a
// layout change that moves a button without moving its hit area fails here.
func TestShelvedNoticeClickPressesTheButtonUnderIt(t *testing.T) {
	for _, size := range [][2]int{{24, 80}, {10, 40}} {
		rows, cols := size[0], size[1]
		t.Run(fmt.Sprintf("%dx%d", rows, cols), func(t *testing.T) {
			n := &shelvedNotice{session: "agent", lines: []string{"$ make", "ok"}}
			screen := paintedScreen(t, n, rows, cols)

			for _, tc := range []struct {
				label string
				want  shelvedChoice
			}{
				{"[ Restore  r ]", shelvedRestore},
				{"[ Detach  d ]", shelvedDetach},
			} {
				row, col := find(t, screen, tc.label)
				if got, ok := n.feed(click(row, col+len(tc.label)/2), nil); !ok || got != tc.want {
					t.Errorf("click on %q = %v, %v; want %v\nscreen:\n%s", tc.label, got, ok, tc.want, screen)
				}
				if got, ok := n.feed(click(row, col-1), nil); ok {
					t.Errorf("click left of %q = %v, want nothing", tc.label, got)
				}
			}
		})
	}
}

// The notice draws the last screen and the box over it, and names the session.
func TestShelvedNoticeShowsTheLastScreen(t *testing.T) {
	n := &shelvedNotice{session: "agent", lines: []string{"$ make", "ok"}}
	screen := paintedScreen(t, n, 24, 80)
	lines := strings.Split(screen, "\n")
	if lines[0] != "$ make" || lines[1] != "ok" {
		t.Errorf("first rows = %q, want the last screen\nscreen:\n%s", lines[:2], screen)
	}
	find(t, screen, "agent is shelved")
}

// paintedScreen runs the notice's paint through a terminal and returns the text on screen.
func paintedScreen(t *testing.T, n *shelvedNotice, rows, cols int) string {
	t.Helper()
	term, err := vt.New(uint16(rows), uint16(cols), vt.Callbacks{})
	if err != nil {
		t.Fatal(err)
	}
	defer term.Close()
	if err := term.Write([]byte(n.paint(rows, cols))); err != nil {
		t.Fatal(err)
	}
	plain, err := term.Plain()
	if err != nil {
		t.Fatal(err)
	}
	return string(plain)
}

// find returns the 1-based row and column of text on a screen.
func find(t *testing.T, screen, text string) (row, col int) {
	t.Helper()
	for i, line := range strings.Split(screen, "\n") {
		if j := strings.Index(line, text); j >= 0 {
			return i + 1, j + 1
		}
	}
	t.Fatalf("%q is not on the screen:\n%s", text, screen)
	return 0, 0
}

// click is a left-button press at a 1-based cell, as a terminal reports it in SGR mode.
func click(row, col int) []byte {
	return fmt.Appendf(nil, "\x1b[<0;%d;%dM", col, row)
}
