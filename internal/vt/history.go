//go:build cgo

package vt

import (
	"errors"

	"go.mitchellh.com/libghostty"
)

// ErrHistoryDiscarded reports that the scrollback a HistoryReader was walking went away under it: pruned past
// the scrollback limit, cleared by ESC [3J, reset, or left behind by a switch between the main and alternate
// screens. What was read is still right; what follows it cannot be found.
var ErrHistoryDiscarded = errors.New("the scrollback was discarded while being read")

// HistoryReader walks a terminal's contents a page at a time, as plain text with soft-wrapped lines rejoined.
type HistoryReader interface {
	// Next returns up to rows screen rows from where the last call stopped, every line ending in a newline,
	// and whether the walk reached the bottom. A row count past what remains returns what remains.
	Next(rows int) (text []byte, done bool, err error)
	// Close releases the reader. Safe to call more than once, and after the terminal has closed.
	Close()
}

// OpenHistory starts a walk from the top of the scrollback.
//
// A page at a time rather than one render, because a render holds the model's lock for the whole of it,
// which blocks the session's output: 68ms for 100000 lines at 200 columns, against about 0.5ms for a page
// of 1000 rows whatever the total. The position is a libghostty tracked reference, which follows its row
// through new output, pruning above it, and reflow on a resize, so a page boundary never repeats or skips
// a line. A row number would: a resize from 200 to 10 columns and back moved the same content across 48,
// 50, and 47 rows.
//
// The walk is not a snapshot. Output arriving during it lands below the position and is read when the walk
// gets there.
func (s *SessionTerminal) OpenHistory() (HistoryReader, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.term.closed {
		return nil, ErrHistoryDiscarded
	}
	alt, err := s.term.OnAltScreen()
	if err != nil {
		return nil, err
	}
	ref, err := s.term.term.TrackGridRef(libghostty.Point{Tag: libghostty.PointTagScreen})
	if err != nil {
		return nil, check(err, "tracking the top of the scrollback")
	}
	return &historyReader{st: s, ref: ref, alt: alt}, nil
}

type historyReader struct {
	st  *SessionTerminal
	ref *libghostty.TrackedGridRef
	// alt is the screen the walk started on. The reference stays on that screen's rows while page ends are
	// resolved against whichever is active, so a switch mid-walk would pair one screen's start with the
	// other's end.
	alt  bool
	done bool
}

func (r *historyReader) Next(rows int) (out []byte, done bool, err error) {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	defer func() { err = recovered(recover(), err, "reading a page of history") }()
	if r.done {
		return nil, true, nil
	}
	if rows <= 0 {
		rows = 1
	}
	t := r.st.term
	if t.closed || r.ref == nil || !r.ref.HasValue() {
		return nil, false, ErrHistoryDiscarded
	}
	if alt, err := t.OnAltScreen(); err != nil {
		return nil, false, err
	} else if alt != r.alt {
		return nil, false, ErrHistoryDiscarded
	}

	p, err := r.ref.Point(libghostty.PointTagScreen)
	if err != nil {
		return nil, false, ErrHistoryDiscarded
	}
	total, err := t.term.TotalRows()
	if err != nil {
		return nil, false, check(err, "reading the row count")
	}
	last := uint32(total) - 1
	end := min(p.Y+uint32(rows)-1, last)
	// A page ends on a hard line break, or joining two pages splits a soft-wrapped line in two. Paging 10000
	// lines at 200 columns split some without this, while a match count over the result still came out right.
	for end < last {
		wrapped, err := t.rowWraps(end)
		if err != nil {
			return nil, false, err
		}
		if !wrapped {
			break
		}
		end++
	}

	_, cols, err := t.Size()
	if err != nil {
		return nil, false, err
	}
	start, err := r.ref.Snapshot()
	if err != nil {
		return nil, false, ErrHistoryDiscarded
	}
	endRef, ok, err := t.gridRef(libghostty.PointTagScreen, cols-1, end)
	if err != nil {
		return nil, false, err
	}
	if !ok {
		return nil, false, ErrHistoryDiscarded
	}
	out, err = t.format(formatOptions{
		emit:      libghostty.FormatterFormatPlain,
		unwrap:    true,
		trim:      true,
		selection: &libghostty.Selection{Start: *start, End: *endRef},
	})
	if err != nil {
		return nil, false, err
	}
	// A selection's text has no newline after its last row, so pages would run together.
	if len(out) > 0 && out[len(out)-1] != '\n' {
		out = append(out, '\n')
	}
	if end >= last {
		r.done = true
		return out, true, nil
	}
	if err := r.ref.Set(t.term, libghostty.Point{Tag: libghostty.PointTagScreen, Y: end + 1}); err != nil {
		return nil, false, check(err, "advancing the history position")
	}
	return out, false, nil
}

func (r *historyReader) Close() {
	r.st.mu.Lock()
	defer r.st.mu.Unlock()
	r.ref.Close()
	r.ref = nil
	r.done = true
}

// rowWraps reports whether a screen row soft-wraps into the next one.
func (t *Terminal) rowWraps(y uint32) (bool, error) {
	ref, ok, err := t.gridRef(libghostty.PointTagScreen, 0, y)
	if err != nil || !ok {
		return false, err
	}
	row, err := ref.Row()
	if err != nil {
		return false, check(err, "reading a row")
	}
	wrapped, err := row.Wrap()
	if err != nil {
		return false, check(err, "reading a row's wrap flag")
	}
	return wrapped, nil
}
