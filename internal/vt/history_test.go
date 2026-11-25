package vt

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// historyTerminal holds lines of varying length at 40 columns, so some soft-wrap and a page boundary can land
// inside one.
func historyTerminal(t *testing.T, lines int) *SessionTerminal {
	t.Helper()
	st, err := NewSessionTerminal(10, 40, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	for i := range lines {
		if err := st.Write(fmt.Appendf(nil, "line %d %s\r\n", i, strings.Repeat("x", i%90))); err != nil {
			t.Fatal(err)
		}
	}
	return st
}

func readAll(t *testing.T, r HistoryReader, rows int) (string, error) {
	t.Helper()
	var out bytes.Buffer
	for {
		page, done, err := r.Next(rows)
		out.Write(page)
		if err != nil || done {
			return out.String(), err
		}
	}
}

// Pages joined are exactly the whole scrollback, unwrapped, whatever the page size: no line repeated, dropped,
// or split at a boundary.
func TestHistoryPagesJoinToTheWhole(t *testing.T) {
	st := historyTerminal(t, 500)
	whole, err := st.Tail(0, true)
	if err != nil {
		t.Fatal(err)
	}
	want := string(whole) + "\n"
	for _, rows := range []int{1, 7, 100, 100000} {
		r, err := st.OpenHistory()
		if err != nil {
			t.Fatal(err)
		}
		got, err := readAll(t, r, rows)
		r.Close()
		if err != nil {
			t.Fatalf("rows=%d: %v", rows, err)
		}
		if got != want {
			t.Errorf("rows=%d: paged history differs from the whole, %d bytes against %d", rows, len(got), len(want))
		}
	}
}

// Output arriving mid-walk is read when the walk gets to it, after everything that was there before.
func TestHistoryContinuesThroughNewOutput(t *testing.T) {
	st := historyTerminal(t, 50)
	r, err := st.OpenHistory()
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	first, _, err := r.Next(5)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Write([]byte("later\r\n")); err != nil {
		t.Fatal(err)
	}
	rest, err := readAll(t, r, 5)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := st.Tail(0, true)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := string(first)+rest, string(whole)+"\n"; got != want {
		t.Errorf("history across new output:\n%q\nwant:\n%q", got, want)
	}
}

// Losing the position is reported rather than skipped past.
func TestHistoryReportsDiscardedScrollback(t *testing.T) {
	for _, tc := range []struct {
		name string
		mid  string
	}{
		{"cleared", "\x1b[3J"},
		{"reset", "\x1bc"},
		{"alternate screen", "\x1b[?1049h"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := historyTerminal(t, 200)
			r, err := st.OpenHistory()
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			if _, _, err := r.Next(5); err != nil {
				t.Fatal(err)
			}
			if err := st.Write([]byte(tc.mid)); err != nil {
				t.Fatal(err)
			}
			if _, _, err := r.Next(5); !errors.Is(err, ErrHistoryDiscarded) {
				t.Errorf("Next after %s = %v, want ErrHistoryDiscarded", tc.name, err)
			}
		})
	}
}

// A reader outliving its terminal fails cleanly, which is what a session ending mid-walk leaves.
func TestHistoryAfterTerminalCloses(t *testing.T) {
	st := historyTerminal(t, 20)
	r, err := st.OpenHistory()
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	if _, _, err := r.Next(5); !errors.Is(err, ErrHistoryDiscarded) {
		t.Errorf("Next after Close = %v, want ErrHistoryDiscarded", err)
	}
	r.Close()
	r.Close()
}
