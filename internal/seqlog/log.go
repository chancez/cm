// Package seqlog provides a bounded, sequence-numbered byte log with live subscribers.
//
// It exists because two layers need the same thing for the same reason. The shim buffers
// pty output so a server that goes away and returns resumes rather than losing output, and
// the server buffers what it has consumed so a reattaching client can be brought up to
// date. In both cases a subscriber names a byte offset and is told whether the bytes it
// asked for are still available.
package seqlog

import (
	"errors"
	"sync"

	"github.com/chancez/cm/internal/seq"
)

// ErrClosed is returned once the log is closed and no further output will arrive.
var ErrClosed = errors.New("output log is closed")

// Log is a bounded, sequence-numbered buffer of terminal output with live subscribers.
//
// It exists so the server can go away and come back without the session noticing.
// Output continues to be accepted while nothing is subscribed, and a returning server
// resumes from the sequence number it last saw rather than resynchronizing. Without it,
// a server restart would either block the shell on a full pty buffer or silently drop
// output.
//
// Sequence numbers count bytes, not writes, so a resume point can fall inside a chunk.
// Numbering writes instead would make "resume from N" ambiguous whenever a chunk was
// split differently on the second pass.
//
// The buffer is bounded and drops from the front. A detached session that keeps
// producing output must not grow without limit, and the oldest output is the least
// valuable: terminal state is reconstructed from the most recent bytes.
type Log[S seq.Number] struct {
	mu sync.Mutex
	// buf holds bytes for sequence numbers [oldest, oldest+len(buf)).
	buf    []byte
	oldest S
	max    int
	closed bool
	// subs are woken on append and on close. Keyed by pointer so a subscriber can
	// remove itself without an index.
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch chan struct{}
}

// New returns a log retaining at most max bytes, numbering from zero. A max below 1 is
// treated as 1 so the invariants below hold without special cases.
func New[S seq.Number](max int) *Log[S] {
	return NewAt[S](max, 0)
}

// NewAt returns a log whose first byte will be numbered start.
//
// A caller relaying another log's output needs this: the server consumes a shim's stream
// beginning at some sequence number and must keep using those numbers, so a position named
// by a client means the same thing on both hops. Numbering from zero would make the two
// disagree by however much the server had already missed.
func NewAt[S seq.Number](max int, start S) *Log[S] {
	if max < 1 {
		max = 1
	}
	return &Log[S]{
		max:    max,
		oldest: start,
		subs:   make(map[*subscriber]struct{}),
	}
}

// Append adds output and wakes subscribers.
//
// A write larger than the whole buffer keeps its tail rather than being rejected: the
// most recent bytes are what matter for reconstructing terminal state, and refusing the
// write would mean losing them instead.
func (l *Log[S]) Append(p []byte) {
	if len(p) == 0 {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}

	if len(p) >= l.max {
		// Advance oldest past everything we are discarding, including the head of p.
		l.oldest += S(len(l.buf)) + S(len(p)-l.max)
		l.buf = append(l.buf[:0], p[len(p)-l.max:]...)
	} else {
		l.buf = append(l.buf, p...)
		if excess := len(l.buf) - l.max; excess > 0 {
			// copy rather than reslice: reslicing would keep the backing array growing
			// as the window advances, so the buffer would retain max bytes but occupy
			// unboundedly more memory.
			l.buf = l.buf[:copy(l.buf, l.buf[excess:])]
			l.oldest += S(excess)
		}
	}

	l.wakeLocked()
}

// Close marks the log complete. Subscribers drain what remains and then see ErrClosed.
func (l *Log[S]) Close() {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return
	}
	l.closed = true
	l.wakeLocked()
}

// wakeLocked signals every subscriber. The channels have capacity 1 and carry no data,
// so a non-blocking send is enough: a subscriber that has not yet observed the previous
// signal will see the new state when it wakes.
func (l *Log[S]) wakeLocked() {
	for s := range l.subs {
		select {
		case s.ch <- struct{}{}:
		default:
		}
	}
}

// Oldest returns the lowest sequence number still retained.
func (l *Log[S]) Oldest() S {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oldest
}

// Next returns the sequence number that will be assigned to the next byte appended.
func (l *Log[S]) Next() S {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oldest + S(len(l.buf))
}

// Bounds returns the oldest retained sequence number and the next sequence number to be
// assigned. Everything in [oldest, next) is currently readable.
func (l *Log[S]) Bounds() (oldest, next S) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oldest, l.oldest + S(len(l.buf))
}

// Snapshot returns the retained bytes from a sequence number, without waiting for more.
//
// Distinct from Subscribe, which exists to follow a stream and blocks for output that has not arrived.
// A caller rendering what a command has printed so far wants the opposite: whatever is there now, and
// no waiting. Using a Reader for that means either blocking forever on a session that has gone quiet or
// guessing a timeout, both of which turn a bounded read into a hang.
//
// gap reports that bytes at or after from had already been dropped, so the result begins later than
// asked. That matters more here than for a follower: replaying from a hole cannot reconstruct the state
// the missing bytes established, and for a read anchored at a command boundary it means the command's
// output has partly aged out of the buffer.
func (l *Log[S]) Snapshot(from S) (data []byte, gap bool) {
	l.mu.Lock()
	defer l.mu.Unlock()

	end := l.oldest + S(len(l.buf))
	if from >= end {
		// Nothing yet. Not an error: a command that has just started has printed nothing, and an empty
		// read is the honest answer.
		return nil, false
	}
	if from < l.oldest {
		from, gap = l.oldest, true
	}
	// Copied rather than aliased, since the buffer is trimmed from the front under this same lock and a
	// retained slice would shift out from under the caller.
	out := make([]byte, uint64(end-from))
	copy(out, l.buf[from-l.oldest:])
	return out, gap
}
