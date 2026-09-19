package client

import (
	"sync"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// maxOutBacklog bounds what a stalled attach loop may accumulate.
//
// Sized for the stall this exists to absorb rather than for a session's lifetime: a terminal that
// stops draining for a moment costs a few screens of output, and a screen is about 20 KB, so this is
// roughly fifty of them. Writing that much to a terminal once it drains again takes about 10ms, which
// is the other half of the choice: a larger backlog turns into a visible replay of stale output, and
// past this point a repaint is both faster and more correct.
const maxOutBacklog = 1 << 20

// outQueue sits between the server's stream and the attach loop, and exists because a ttrpc stream can
// no longer be left waiting.
//
// ttrpc v1.2.9 buffers 64 messages per stream and closes it with ErrStreamFull if the consumer has not
// drained within a second; v1.2.7 blocked instead, which is the behavior every consumer here was
// written against. See TestASlowConsumerLosesTheStream in internal/transport for the measurement.
//
// The attach loop stalls for longer than a second in ordinary use. It writes to the terminal inline,
// and a terminal that is not draining blocks the write -- one kitty that stops reading stalls every cm
// client in every one of its windows, which is how two sessions came to reconnect 33ms apart. It also
// holds the loop while the session picker is open, for as long as somebody is looking at it.
//
// So the stream is drained into this queue by a goroutine that never blocks, and the stall is paid for
// in memory instead of in the stream's life. Past maxOutBacklog the backlog is dropped and a repaint is
// owed, which is the same recovery a gap in the server's log already asks for.
type outQueue struct {
	mu sync.Mutex
	// msgs is FIFO. Held as a slice rather than a channel because a channel cannot be over-filled
	// without blocking the sender, which is the whole problem.
	msgs []outMsg
	// bytes is what msgs holds, for the cap.
	bytes int
	// dropped means the cap was passed, so a repaint is owed and nothing buffered can matter. Sticky:
	// the consumer is by definition not reading while this is set, and clearing it before it has been
	// told would lose the repaint.
	dropped bool
	// wake carries one signal, so a consumer selecting on it learns there is something without the
	// producer ever waiting.
	wake chan struct{}
}

func newOutQueue() *outQueue {
	return &outQueue{wake: make(chan struct{}, 1)}
}

// push adds a message and never blocks.
func (q *outQueue) push(m outMsg) {
	q.mu.Lock()
	if q.dropped {
		// A repaint is already owed. Buffering more of what is about to be thrown away is how a
		// stalled client's memory would grow without bound.
		q.mu.Unlock()
		return
	}
	q.msgs = append(q.msgs, m)
	q.bytes += outMsgBytes(m)
	if q.bytes > maxOutBacklog {
		// The backlog is replaced rather than trimmed. A repaint supersedes every chunk waiting here,
		// and handing the loop a prefix of them would paint output against a screen state that the
		// dropped remainder was going to establish.
		q.msgs = append(q.msgs[:0], outMsg{dropped: true})
		q.bytes = 0
		q.dropped = true
	}
	q.mu.Unlock()
	q.signal()
}

// pop takes the oldest message, reporting false when there is none.
func (q *outQueue) pop() (outMsg, bool) {
	q.mu.Lock()
	if len(q.msgs) == 0 {
		q.mu.Unlock()
		return outMsg{}, false
	}
	m := q.msgs[0]
	q.msgs = q.msgs[1:]
	q.bytes -= outMsgBytes(m)
	more := len(q.msgs) > 0
	q.mu.Unlock()
	if more {
		// Re-signalled so the consumer comes back for the rest. One signal per message is what lets the
		// attach loop keep handling a message per iteration of its select, as it did when this was a
		// channel.
		q.signal()
	}
	return m, true
}

func (q *outQueue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// outMsgBytes is what a message costs to hold. Only the payloads that can be large are counted: the
// point is to bound memory, not to bill exactly.
func outMsgBytes(m outMsg) int {
	if m.resp == nil {
		return 0
	}
	n := len(m.resp.GetOutput().GetData())
	n += len(m.resp.GetImages().GetData())
	n += len(m.resp.GetOpened().GetRestore())
	n += len(m.resp.GetQuery().GetData())
	return n
}

// outMsg is one message from the server, the error that ended the stream, or notice that output was
// dropped because this client could not keep up.
type outMsg struct {
	resp    *serverv1.AttachResponse
	err     error
	dropped bool
}
