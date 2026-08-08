package shim

import (
	"errors"
	"sync"
)

// ErrLogClosed is returned once the log is closed and no further output will arrive.
var ErrLogClosed = errors.New("output log is closed")

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
type Log struct {
	mu sync.Mutex
	// buf holds bytes for sequence numbers [oldest, oldest+len(buf)).
	buf    []byte
	oldest uint64
	max    int
	closed bool
	// subs are woken on append and on close. Keyed by pointer so a subscriber can
	// remove itself without an index.
	subs map[*subscriber]struct{}
}

type subscriber struct {
	ch chan struct{}
}

// NewLog returns a log retaining at most max bytes. A max below 1 is treated as 1 so
// the invariants below hold without special cases.
func NewLog(max int) *Log {
	if max < 1 {
		max = 1
	}
	return &Log{
		max:  max,
		subs: make(map[*subscriber]struct{}),
	}
}

// Append adds output and wakes subscribers.
//
// A write larger than the whole buffer keeps its tail rather than being rejected: the
// most recent bytes are what matter for reconstructing terminal state, and refusing the
// write would mean losing them instead.
func (l *Log) Append(p []byte) {
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
		l.oldest += uint64(len(l.buf)) + uint64(len(p)-l.max)
		l.buf = append(l.buf[:0], p[len(p)-l.max:]...)
	} else {
		l.buf = append(l.buf, p...)
		if excess := len(l.buf) - l.max; excess > 0 {
			// copy rather than reslice: reslicing would keep the backing array growing
			// as the window advances, so the buffer would retain max bytes but occupy
			// unboundedly more memory.
			l.buf = l.buf[:copy(l.buf, l.buf[excess:])]
			l.oldest += uint64(excess)
		}
	}

	l.wakeLocked()
}

// Close marks the log complete. Subscribers drain what remains and then see ErrLogClosed.
func (l *Log) Close() {
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
func (l *Log) wakeLocked() {
	for s := range l.subs {
		select {
		case s.ch <- struct{}{}:
		default:
		}
	}
}

// Bounds returns the oldest retained sequence number and the next sequence number to be
// assigned. Everything in [oldest, next) is currently readable.
func (l *Log) Bounds() (oldest, next uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.oldest, l.oldest + uint64(len(l.buf))
}
