package seqlog

import (
	"context"

	"github.com/chancez/cm/internal/seq"
)

// Chunk is a run of output bytes and the sequence number of its first byte.
type Chunk[S seq.Number] struct {
	Seq  S
	Data []byte
	// Gap reports that bytes before Seq were dropped before this reader could read
	// them, so its view is not continuous with what it last saw.
	//
	// The distinction matters: a receiver reconstructing terminal state by replaying
	// bytes cannot simply continue across a hole, because the escape sequences that
	// established the current screen may be part of what was lost. Seeing Gap means
	// resynchronize, not continue.
	Gap bool
}

// Reader streams chunks from a Log starting at a sequence number, blocking for more
// until the log closes or the context is cancelled.
// A Reader may be closed while another goroutine is blocked in Next, which is the normal shape of a
// client detaching, so sub is guarded by the log's mutex rather than only read at setup.
type Reader[S seq.Number] struct {
	log *Log[S]
	// sub is nil once closed. Read and written under log.mu.
	sub *subscriber
	// next is the sequence number this reader wants next.
	next S
	// gap is set when the next chunk must be flagged as discontinuous.
	gap bool
}

// Subscribe returns a Reader positioned at from.
//
// A from beyond the log's end is clamped to the end rather than rejected, so a
// subscriber that saw output the log has since forgotten, which can happen if the log
// was reset, simply continues from the present. That clamp is also flagged with Gap,
// because the two situations it covers are indistinguishable from here and one of them
// loses bytes.
//
// The benign case is a log that was reset behind the subscriber, where continuing from
// the present is exactly right and there is nothing missing. The other is a position
// expressed in a different numbering than this log's, which is a real defect and used to
// be silent: a client resuming across a server restart asked for a position counted in
// post-rewrite bytes while the new log began at the shim's count, so it was clamped
// forward past output it never received. Whatever escape sequence straddled that point
// arrived with its front sliced off, which renders as literal text and looks like a
// terminal bug rather than a lost prefix.
//
// Flagging it does not repair the drift, and is not meant to. It converts "silently skip
// bytes" into "tell the reader its view is discontinuous", which a client already knows
// how to handle by resynchronizing rather than by continuing.
//
// The numbering mistake itself is now a compile error rather than something to detect: S ties a log
// to one sequence space, so a position from the other one cannot be passed here. See internal/seq.
// The clamp and the flag remain for the benign case, a log genuinely reset behind a subscriber.
//
// A from before the oldest retained byte is served from the oldest instead, and is
// likewise flagged with Gap.
func (l *Log[S]) Subscribe(from S) *Reader[S] {
	l.mu.Lock()
	defer l.mu.Unlock()

	s := &subscriber{ch: make(chan struct{}, 1)}
	l.subs[s] = struct{}{}

	next, end := from, l.oldest+S(len(l.buf))
	gap := false
	if next < l.oldest {
		next, gap = l.oldest, true
	} else if next > end {
		next, gap = end, true
	}

	// Prime the channel so a reader that already has data available does not wait for
	// the next append to notice.
	select {
	case s.ch <- struct{}{}:
	default:
	}

	return &Reader[S]{log: l, sub: s, next: next, gap: gap}
}

// Close releases the reader's registration. Safe to call more than once, and safe to call while
// another goroutine is blocked in Next.
//
// sub is both checked and cleared under the lock. Reading it first and clearing it after, which is
// the obvious way to write this, races a concurrent Next: Next would see a non-nil sub, and by the
// time it dereferenced it Close had nilled it, so a detaching client crashed the server with a nil
// dereference. Rare in practice, because it needs the detach to land inside a window a few
// instructions wide, and a crash rather than a wrong answer when it does.
func (r *Reader[S]) Close() {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	if r.sub == nil {
		return
	}
	// Wake a Next that is already blocked on this subscription before removing it. Without this the
	// reader is unregistered, so no append or log close will ever signal it again, and Next waits
	// forever on a channel nobody holds. That is worse than the crash this ordering also prevents: a
	// leaked goroutine per detached client, holding the session's log alive.
	//
	// A non-blocking send is enough. The channel has capacity 1 and carries no data, so a signal
	// already pending will wake Next just as well.
	select {
	case r.sub.ch <- struct{}{}:
	default:
	}

	delete(r.log.subs, r.sub)
	r.sub = nil
}

// Next returns the next available chunk, blocking until there is one.
//
// It returns ErrClosed only after all remaining buffered output has been returned, so
// a subscriber that attaches after the shell exits still sees the final output rather
// than an immediate error.
//
// The returned Data is a fresh copy and is safe to retain.
func (r *Reader[S]) Next(ctx context.Context) (Chunk[S], error) {
	for {
		r.log.mu.Lock()

		// Bytes this reader has already passed may have been dropped while it was
		// working, so re-check the lower bound every time rather than only at subscribe.
		if r.next < r.log.oldest {
			r.next, r.gap = r.log.oldest, true
		}

		end := r.log.oldest + S(len(r.log.buf))
		if r.next < end {
			off := int(r.next - r.log.oldest)
			data := make([]byte, len(r.log.buf)-off)
			copy(data, r.log.buf[off:])
			c := Chunk[S]{Seq: r.next, Data: data, Gap: r.gap}
			r.next = end
			r.gap = false
			r.log.mu.Unlock()
			return c, nil
		}

		closed := r.log.closed
		// A reader closed while blocked here has no subscription left to wait on. Reported as
		// ErrClosed, the same as a closed log, since either way nothing further is coming.
		if r.sub == nil {
			r.log.mu.Unlock()
			return Chunk[S]{}, ErrClosed
		}
		ch := r.sub.ch
		r.log.mu.Unlock()

		if closed {
			return Chunk[S]{}, ErrClosed
		}

		select {
		case <-ch:
		case <-ctx.Done():
			return Chunk[S]{}, ctx.Err()
		}
	}
}

// Position returns the sequence number the reader will deliver next. A client records this
// so it can resume from the same place after a reconnect.
func (r *Reader[S]) Position() S {
	r.log.mu.Lock()
	defer r.log.mu.Unlock()
	return r.next
}
