package vt

import (
	"fmt"
	"strings"
	"testing"
)

// scrollbackRows writes n numbered lines of 100 characters and reports how many rows of scrollback the
// model kept.
func scrollbackRows(t *testing.T, limit, n int) uint {
	t.Helper()
	st, err := NewSessionTerminal(50, 200, limit)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	pad := strings.Repeat("x", 100)
	for i := range n {
		if err := st.Write(fmt.Appendf(nil, "line %06d %s\r\n", i, pad)); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := st.term.term.ScrollbackRows()
	if err != nil {
		t.Fatal(err)
	}
	return rows
}

// scrollback.lines is the only limit on retained history. libghostty also has a byte limit, whose default
// is about one page, and left at that it kept 197 rows at 200 columns whatever scrollback.lines said,
// unlimited included. Every restore, cm history, cm read, and the overlay pager showed that page.
func TestScrollbackLinesGovernRetention(t *testing.T) {
	// Unlimited keeps everything: 5000 lines plus the cursor's empty row, less the 50 on screen.
	if got := scrollbackRows(t, 0, 5000); got != 4951 {
		t.Errorf("unlimited scrollback kept %d rows, want 4951", got)
	}
	// A limit is approximate in both directions, since libghostty prunes whole pages: 2000 kept 1857 here,
	// and a page at this width is about 200 rows.
	if got := scrollbackRows(t, 2000, 5000); got < 1600 || got > 2400 {
		t.Errorf("scrollback.lines=2000 kept %d rows, want within a page or two of 2000", got)
	}
}
