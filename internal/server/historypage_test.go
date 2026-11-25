package server

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"testing/synctest"
	"time"

	"github.com/chancez/cm/internal/vt"
)

func historySession(id, written string) (*Session, *fakeTerminal) {
	term := &fakeTerminal{written: []byte(written)}
	return &Session{id: id, done: make(chan struct{}), term: term}, term
}

// A walk returns every line once, in order, hands back the same cursor until it finishes, and lets go of
// the reader and the cursor at the end.
func TestHistoryPageWalksToTheEnd(t *testing.T) {
	ctx := context.Background()
	m := &Manager{}
	sess, term := historySession("w", "a\nb\nc\n")

	first, err := m.HistoryPage(ctx, "w", sess, "", 2)
	if err != nil {
		t.Fatal(err)
	}
	if first.Cursor == "" {
		t.Fatalf("first page = %+v, want a cursor", first)
	}
	if want := (HistoryPage{Data: []byte("a\nb\n"), Cursor: first.Cursor}); !reflect.DeepEqual(first, want) {
		t.Fatalf("first page = %+v, want %+v", first, want)
	}
	second, err := m.HistoryPage(ctx, "w", sess, first.Cursor, 2)
	if err != nil {
		t.Fatal(err)
	}
	if want := (HistoryPage{Data: []byte("c\n"), Done: true}); !reflect.DeepEqual(second, want) {
		t.Errorf("last page = %+v, want %+v", second, want)
	}
	if _, err := m.HistoryPage(ctx, "w", sess, first.Cursor, 2); !errors.Is(err, errUnknownCursor) {
		t.Errorf("page after done = %v, want errUnknownCursor", err)
	}
	if term.closedReaders != 1 {
		t.Errorf("readers closed = %d, want 1", term.closedReaders)
	}
}

// A discarded walk is reported in the page, not as an error, and its cursor is gone.
func TestHistoryPageReportsDiscarded(t *testing.T) {
	ctx := context.Background()
	m := &Manager{}
	sess, term := historySession("w", "a\nb\nc\n")
	first, err := m.HistoryPage(ctx, "w", sess, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	term.mu.Lock()
	term.historyErr = vt.ErrHistoryDiscarded
	term.mu.Unlock()
	got, err := m.HistoryPage(ctx, "w", sess, first.Cursor, 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := (HistoryPage{Discarded: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("page after discard = %+v, want %+v", got, want)
	}
	if _, err := m.HistoryPage(ctx, "w", sess, first.Cursor, 1); !errors.Is(err, errUnknownCursor) {
		t.Errorf("page after discard = %v, want errUnknownCursor", err)
	}
}

// A cursor reads only the session it was opened on.
func TestHistoryPageRefusesAnotherSessionsCursor(t *testing.T) {
	ctx := context.Background()
	m := &Manager{}
	a, _ := historySession("a", "1\n2\n")
	b, _ := historySession("b", "3\n4\n")
	page, err := m.HistoryPage(ctx, "a", a, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.HistoryPage(ctx, "b", b, page.Cursor, 1); !errors.Is(err, errUnknownCursor) {
		t.Errorf("a's cursor on b = %v, want errUnknownCursor", err)
	}
}

// A session without a model has nothing to walk, which is a finished walk rather than an error.
func TestHistoryPageWithoutAModel(t *testing.T) {
	m := &Manager{}
	sess := &Session{id: "w", done: make(chan struct{})}
	got, err := m.HistoryPage(context.Background(), "w", sess, "", 1)
	if err != nil {
		t.Fatal(err)
	}
	if want := (HistoryPage{Done: true}); !reflect.DeepEqual(got, want) {
		t.Errorf("page = %+v, want %+v", got, want)
	}
}

// Open walks of one session are capped, and an abandoned one frees its slot by expiring.
func TestHistoryPageCapsAndExpiresCursors(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		m := &Manager{}
		defer m.cursors.closeAll()
		sess, term := historySession("w", "a\nb\nc\n")

		var cursors []string
		for range maxHistoryCursorsPerSession {
			page, err := m.HistoryPage(ctx, "w", sess, "", 1)
			if err != nil {
				t.Fatal(err)
			}
			cursors = append(cursors, page.Cursor)
		}
		if _, err := m.HistoryPage(ctx, "w", sess, "", 1); err == nil {
			t.Fatal("a walk past the cap was opened")
		}
		// The refused walk's reader was opened and closed; count expiries from here.
		term.mu.Lock()
		refused := term.closedReaders
		term.mu.Unlock()

		// Paging resets the idle clock, so a cursor in use outlives one left alone.
		time.Sleep(historyCursorIdle / 2)
		if _, err := m.HistoryPage(ctx, "w", sess, cursors[0], 1); err != nil {
			t.Fatal(err)
		}
		time.Sleep(historyCursorIdle/2 + time.Second)
		synctest.Wait()

		term.mu.Lock()
		closed := term.closedReaders - refused
		term.mu.Unlock()
		if closed != maxHistoryCursorsPerSession-1 {
			t.Errorf("readers closed by expiry = %d, want %d", closed, maxHistoryCursorsPerSession-1)
		}
		if _, err := m.HistoryPage(ctx, "w", sess, cursors[1], 1); !errors.Is(err, errUnknownCursor) {
			t.Errorf("expired cursor = %v, want errUnknownCursor", err)
		}
		if _, err := m.HistoryPage(ctx, "w", sess, cursors[0], 1); err != nil {
			t.Errorf("cursor in use expired: %v", err)
		}
		if _, err := m.HistoryPage(ctx, "w", sess, "", 1); err != nil {
			t.Errorf("no walk opened after expiry freed slots: %v", err)
		}
	})
}
