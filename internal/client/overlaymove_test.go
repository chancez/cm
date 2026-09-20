package client

import (
	"errors"
	"strings"
	"testing"
)

// ringItems is a session list with one of them marked as the one this client is on, which is what the move
// keys count from.
func ringItems(n, current int) []pickItem {
	items := pickItems(n)
	items[current].Current = true
	return items
}

// n steps to the session after this one and wraps at the end, which is the case it is mostly used in: two
// sessions and no wrap means n works once and then stops.
func TestOverlayNextSessionWraps(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()

	if got := o.feed([]byte("n")); !sameResponse(got, overlayResponse{List: true}) {
		t.Fatalf("feed(n) = %+v, want a request for the session list", got)
	}
	if o.mode != overlayMove {
		t.Fatalf("mode = %v, want overlayMove", o.mode)
	}
	// Something on screen while the list is in flight, for the reason the chooser has one: a blank bar
	// during a round trip reads as a keypress that did nothing.
	if rows := o.rows(24, 80); len(rows) != 1 || !strings.Contains(rows[0], "next session") {
		t.Errorf("rows while loading = %q, want a note that the next session is being looked for", rows)
	}

	if got := o.sessions(ringItems(3, 1), nil); !sameResponse(got, overlayResponse{SwitchTo: "@id2", Repaint: true}) {
		t.Errorf("the list arriving = %+v, want a switch to the session after this one", got)
	}

	// From the last session, round to the first.
	o, _ = newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("n"))
	if got := o.sessions(ringItems(3, 2), nil); !sameResponse(got, overlayResponse{SwitchTo: "@id0", Repaint: true}) {
		t.Errorf("n on the last session = %+v, want a wrap to the first", got)
	}
}

// p is the same ring in the other direction, and wraps at the start.
func TestOverlayPreviousSessionWraps(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("p"))
	if got := o.sessions(ringItems(3, 1), nil); !sameResponse(got, overlayResponse{SwitchTo: "@id0", Repaint: true}) {
		t.Errorf("p = %+v, want a switch to the session before this one", got)
	}

	o, _ = newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("p"))
	if got := o.sessions(ringItems(3, 0), nil); !sameResponse(got, overlayResponse{SwitchTo: "@id2", Repaint: true}) {
		t.Errorf("p on the first session = %+v, want a wrap to the last", got)
	}
}

// One session is not a ring. Said rather than switched to itself, which would be a visible repaint for
// nothing, and said rather than silent, which reads as a broken key.
func TestOverlayNextSaysWhenThereIsNowhereToGo(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("n"))

	if got := o.sessions(ringItems(1, 0), nil); !sameResponse(got, overlayResponse{}) {
		t.Errorf("n with one session = %+v, want nothing to do", got)
	}
	if rows := o.rows(24, 80); len(rows) != 1 || !strings.Contains(rows[0], "no other session") {
		t.Errorf("rows = %q, want a line saying there is nowhere to go", rows)
	}
}

// l goes back to the session this window came from, which is what makes it a toggle: the switch it performs
// is what the loop then remembers as the way back.
func TestOverlayLastVisitedSwitchesBack(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.lastSession = "@id0"
	o.open()

	if got := o.feed([]byte("l")); !sameResponse(got, overlayResponse{List: true}) {
		t.Fatalf("feed(l) = %+v, want a request for the session list", got)
	}
	if got := o.sessions(ringItems(3, 2), nil); !sameResponse(got, overlayResponse{SwitchTo: "@id0", Repaint: true}) {
		t.Errorf("the list arriving = %+v, want a switch back to the remembered session", got)
	}
}

// On the first session of a window there is nowhere to go back to, and the key says so rather than doing
// nothing: this is the one action whose availability depends on what the user has already done.
func TestOverlayLastVisitedSaysWhenThereIsNoHistory(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()

	if got := o.feed([]byte("l")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("feed(l) with no history = %+v, want nothing to do, not a list nobody can use", got)
	}
	if rows := o.rows(24, 80); len(rows) != 1 || !strings.Contains(rows[0], "no session visited before") {
		t.Errorf("rows = %q, want a line saying there is no history", rows)
	}
}

// A session remembered can have ended since. The list is what says so, which is why l asks for one instead
// of switching to the reference it holds: an Open for a session that is gone leaves a window with nothing
// to draw.
func TestOverlayLastVisitedSaysWhenItHasEnded(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.lastSession = "@gone"
	o.open()
	o.feed([]byte("l"))

	if got := o.sessions(ringItems(2, 0), nil); !sameResponse(got, overlayResponse{}) {
		t.Errorf("a list without the remembered session = %+v, want nothing to do", got)
	}
	if rows := o.rows(24, 80); len(rows) != 1 || !strings.Contains(rows[0], "has ended") {
		t.Errorf("rows = %q, want a line saying the session is gone", rows)
	}
}

// A failed list is reported rather than switched blindly, since the answer is what the move depends on.
func TestOverlayMoveReportsAFailedList(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("n"))

	if got := o.sessions(nil, errors.New("server is away")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("a failed list = %+v, want nothing to do", got)
	}
	if rows := o.rows(24, 80); len(rows) != 1 || !strings.Contains(rows[0], "server is away") {
		t.Errorf("rows = %q, want the failure on the bar", rows)
	}
}

// Escaping out of a move discards the answer still in flight. Without that, a list arriving after the user
// changed their mind would move the window, which is the chooser's late-answer bug in the other form.
func TestOverlayMoveDiscardsALateAnswer(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("n"))
	o.feed([]byte{0x1b})

	if o.mode != overlayArmed {
		t.Fatalf("mode after escape = %v, want overlayArmed", o.mode)
	}
	if got := o.sessions(ringItems(3, 1), nil); !sameResponse(got, overlayResponse{}) {
		t.Errorf("a list arriving after an escape = %+v, want it dropped", got)
	}
}
