package client

import (
	"sync"
	"time"
)

// pendingQueries remembers the questions this client has been handed, so it can re-offer them when its stream
// drops and it reconnects.
//
// It exists because the server's record of an outstanding question is in its memory, and adoption resubscribes
// from where the old server stopped rather than re-reading the bytes that carried the query. So a restart
// forgets the question, the reply matches nothing and is discarded, and the asking program gets no answer. The
// client is the only thing that still knows, because it was handed the bytes and wrote them to its terminal.
//
// It cannot prune accurately, and that is the shape of the problem rather than a shortcut. The client forwards
// input without classifying it, and which question a given reply answers is decided by the server against the
// query's own bytes, so the client never learns that a particular question has been settled. Two bounds stand
// in for that.
type pendingQueries struct {
	mu      sync.Mutex
	entries []pendingQuery
	now     func() time.Time
}

type pendingQuery struct {
	data  []byte
	asked time.Time
}

// queryMemory is how long a question is worth re-offering.
//
// Long enough to cover a server restart, which is what this exists for and which takes seconds: an upgrade
// stops one server, starts another, and waits for clients to come back. Short enough that a question from
// earlier in a session is not re-offered later, where it would hold the reply queue for the server's own
// requestTimeout and answer nothing.
//
// Deliberately much larger than that requestTimeout of 500ms. The two measure different things: that one is how
// long a client has to answer, and this is how long the client remembers being asked at all. A stale re-offer
// costs one requestTimeout of queue delay and then expires, which is the same outcome as a client that never
// answers.
const queryMemory = 10 * time.Second

// maxPendingQueries bounds how many are kept.
//
// A program can ask several questions at once, and a client that never answers would otherwise accumulate one
// per query for queryMemory. Eight is far more than any real program has outstanding, and the oldest is dropped
// first, because a newer question is the one more likely still to be waiting.
const maxPendingQueries = 8

func (p *pendingQueries) add(data []byte) {
	if len(data) == 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	p.entries = append(p.entries, pendingQuery{
		data:  append([]byte(nil), data...),
		asked: p.clock(),
	})
	if len(p.entries) > maxPendingQueries {
		p.entries = p.entries[len(p.entries)-maxPendingQueries:]
	}
}

// take returns the questions worth re-offering and forgets them.
//
// Consumed rather than read, so one drop re-offers each question once. A client that keeps reconnecting would
// otherwise re-offer the same question every time, holding the reply queue on every attempt.
func (p *pendingQueries) take() [][]byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.entries) == 0 {
		return nil
	}
	cutoff := p.clock().Add(-queryMemory)
	var out [][]byte
	for _, e := range p.entries {
		if e.asked.Before(cutoff) {
			continue
		}
		out = append(out, e.data)
	}
	p.entries = nil
	return out
}

func (p *pendingQueries) clock() time.Time {
	if p.now != nil {
		return p.now()
	}
	return time.Now()
}
