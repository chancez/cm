package shim

import (
	"context"
)

// Chunk is a run of output bytes and the sequence number of its first byte.
type Chunk struct {
	Seq  uint64
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
type Reader struct {
	log *Log
	sub *subscriber
	// next is the sequence number this reader wants next.
	next uint64
	// gap is set when the next chunk must be flagged as discontinuous.
	gap bool
}

// Subscribe returns a Reader positioned at from.
//
// A from beyond the log's end is clamped to the end rather than rejected, so a
// subscriber that saw output the log has since forgotten, which can happen if the log
// was reset, simply continues from the present.
//
// A from before the oldest retained byte is served from the oldest instead, and the
// first chunk is flagged with Gap.
func (l *Log) Subscribe(from uint64) *Reader {
	l.mu.Lock()
	defer l.mu.Unlock()

	s := &subscriber{ch: make(chan struct{}, 1)}
	l.subs[s] = struct{}{}

	next, end := from, l.oldest+uint64(len(l.buf))
	gap := false
	if next < l.oldest {
		next, gap = l.oldest, true
	} else if next > end {
		next = end
	}

	// Prime the channel so a reader that already has data available does not wait for
	// the next append to notice.
	select {
	case s.ch <- struct{}{}:
	default:
	}

	return &Reader{log: l, sub: s, next: next, gap: gap}
}

// Close releases the reader's registration. Safe to call more than once.
func (r *Reader) Close() {
	if r.sub == nil {
		return
	}
	r.log.mu.Lock()
	delete(r.log.subs, r.sub)
	r.log.mu.Unlock()
	r.sub = nil
}

// Next returns the next available chunk, blocking until there is one.
//
// It returns ErrLogClosed only after all remaining buffered output has been returned, so
// a subscriber that attaches after the shell exits still sees the final output rather
// than an immediate error.
//
// The returned Data is a fresh copy and is safe to retain.
func (r *Reader) Next(ctx context.Context) (Chunk, error) {
	for {
		r.log.mu.Lock()

		// Bytes this reader has already passed may have been dropped while it was
		// working, so re-check the lower bound every time rather than only at subscribe.
		if r.next < r.log.oldest {
			r.next, r.gap = r.log.oldest, true
		}

		end := r.log.oldest + uint64(len(r.log.buf))
		if r.next < end {
			off := int(r.next - r.log.oldest)
			data := make([]byte, len(r.log.buf)-off)
			copy(data, r.log.buf[off:])
			c := Chunk{Seq: r.next, Data: data, Gap: r.gap}
			r.next = end
			r.gap = false
			r.log.mu.Unlock()
			return c, nil
		}

		closed := r.log.closed
		ch := r.sub.ch
		r.log.mu.Unlock()

		if closed {
			return Chunk{}, ErrLogClosed
		}

		select {
		case <-ch:
		case <-ctx.Done():
			return Chunk{}, ctx.Err()
		}
	}
}
