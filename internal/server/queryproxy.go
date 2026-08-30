package server

import (
	"context"
	"time"

	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/query"
)

// requestTimeout bounds how long a proxied query waits for a client's answer.
//
// A bound is required rather than optional, because the client on the other end may never answer: a
// terminal that does not implement OSC 52, a client whose window is gone but whose connection has not
// noticed yet, or a read-only follower that cannot reply at all. Without a bound the reply queue behind
// it never drains and every later query on that pty stalls, which converts one unanswerable question into
// a wedged session.
//
// 500ms, matching tmux's INPUT_REQUEST_TIMEOUT. The round trip is a local socket and a terminal's own
// reply, so a client that is going to answer answers in single-digit milliseconds; this is two orders of
// magnitude above that, which is the right side to err on. The cost when it expires is that the asking
// program gets no answer to *that* query, which is exactly what happens today for every terminal-only
// query, so expiry is never worse than the current behavior.
const requestTimeout = 500 * time.Millisecond

// requestSweep is how often expired requests are collected.
//
// A sweep rather than a timer per request: queries arrive in bursts from a prompt hook, and a goroutine
// each would be mostly idle. tmux polls at the same interval for the same reason.
const requestSweep = 100 * time.Millisecond

// pendingRequest is one terminal-only query cm has asked a client to answer, or one locally-generated
// reply waiting its turn behind such a query.
//
// The two live in one queue on purpose, and that is the whole ordering mechanism. A program that asks two
// questions expects the answers in order, and cm can answer some questions immediately while others need
// a round trip to a client. Delivering the fast one first reorders the conversation, and a program reading
// replies positionally then takes the wrong answer for its question. That is the recorded `wallfacer -h`
// corruption: cm injected a cursor report while wallfacer was mid-read on OSC 11, wallfacer consumed it as
// its own answer, and the real OSC 11 reply arrived unclaimed and was printed by the line editor.
type pendingRequest struct {
	// proxied is set when this entry is a question sent to a client and awaiting its reply. When false
	// this is a reply cm has already produced, queued only to preserve order.
	proxied bool
	// tok names the client the question went to, so a reply arriving from a different client is not
	// mistaken for the answer. Nil for a queued local reply.
	tok *attachToken
	// seq is the query itself, kept so a reply can be matched to the question rather than only to the
	// client. The client answers questions cm never asked it too, because cm forwards the queries its own
	// model handles to every client verbatim, and those answers arrive on the same path. See
	// query.AnswersQuery. Empty for a queued local reply.
	seq []byte
	// data is the bytes to write to the pty: the reply, once known. Empty for a proxied entry until its
	// answer arrives.
	data []byte
	// asked is when this entry was created, for expiry.
	asked time.Time
}

// noteQueries inspects a chunk of session output and sends any terminal-only query to a client.
//
// Called from the pump with the shell's original bytes, before the model has consumed them. The order
// matters: a question has to be registered as outstanding *before* the model produces the reply to any
// later answerable query in the same chunk, or that reply would be written straight to the pty and
// overtake the question it was supposed to follow.
//
// Queries cm can answer itself are deliberately not touched here. They are answered by the emulator in the
// ordinary way and picked up by drainPending, which consults this queue to decide whether the reply may go
// out now or must wait.
func (s *Session) noteQueries(data []byte) {
	// Scanned without allocating in the common case, since most chunks contain no queries at all and this
	// is on the path every byte of session output takes.
	for i := 0; i < len(data); {
		n, kind := query.Classify(data[i:])
		if kind == query.TerminalOnly {
			s.proxyQuery(data[i : i+n])
		}
		if n <= 0 {
			// Not a sequence, or an incomplete one at the end of the chunk. Advance a byte: classifying a
			// fragment would risk treating the tail of ordinary output as a question.
			i++
			continue
		}
		i += n
	}
}

// proxyQuery asks one attached client a question cm cannot answer, recording it as outstanding.
//
// Nothing happens when no client can answer. That is not a silent failure but the honest outcome: cm does
// not know the value, so there is no reply to give, and the asking program is in exactly the position it
// is in today. Recording a request with nobody to answer it would stall the queue for the timeout and
// achieve nothing.
func (s *Session) proxyQuery(seq []byte) {
	s.mu.Lock()
	tok := s.queryTargetLocked()
	if tok == nil {
		s.mu.Unlock()
		return
	}
	ch := s.queries[tok]
	if ch == nil {
		s.mu.Unlock()
		return
	}
	s.requests = append(s.requests, &pendingRequest{
		proxied: true,
		tok:     tok,
		seq:     append([]byte(nil), seq...),
		asked:   s.now(),
	})
	s.mu.Unlock()

	// Sent without holding mu, since the channel write can block if the client's stream is slow, and mu
	// guards the output pump's metadata.
	//
	// Dropped rather than waited on when the buffer is full. A client that cannot keep up with questions
	// is one whose answers would arrive too late to be useful anyway, and blocking the pump on it would
	// stall the whole session's output.
	select {
	case ch <- append([]byte(nil), seq...):
	default:
		s.log.Debug("dropping a proxied query, the client's queue is full",
			"session", s.label, "bytes", len(seq))
	}
}

// queryTargetLocked picks the client to ask, or nil when none can answer.
//
// The most recently active interactive client, which is tmux's choice and differs from the answerer
// election this replaces. That election wanted *stability*, because it designated a client to answer on
// its own for an indefinite period, so a moving pick meant a program's consecutive questions were answered
// by different terminals. Here cm asks a specific client a specific question and matches the reply, so
// there is nothing to keep stable, and recency is better: it is the window the user is actually looking
// at, and therefore the one whose colours and clipboard are the ones they mean.
//
// A read-only follower is excluded because its input is dropped on the way back (see recvLoop), so asking
// one guarantees a timeout. A reservation that has not attached is excluded because it has no stream to
// carry the question.
func (s *Session) queryTargetLocked() *attachToken {
	var best *attachToken
	var bestOrder uint64
	for tok, cs := range s.clientSizes {
		if cs.readOnly || !cs.attached {
			continue
		}
		if s.queries[tok] == nil {
			continue
		}
		// Highest order is the most recent attach. Activity would be better still, and order is the
		// closest thing this struct already tracks; revisit if it matters.
		if best == nil || cs.order > bestOrder {
			best, bestOrder = tok, cs.order
		}
	}
	return best
}

// answerFromClient records a reply a client sent for a question cm asked it.
//
// The reply is not written to the pty here. It is stored against the outstanding request and the queue is
// then drained in order, which is what keeps a program's answers in the sequence it asked its questions.
//
// A reply that matches no outstanding request is discarded. That is the case this whole design exists to
// make safe: a client whose terminal answered something on its own, or a client replaying a query out of
// the log after reconnecting, produces bytes nobody asked for, and forwarding them is the duplicate answer
// that printed a branch name into a prompt.
//
// Two things beyond the client's identity have to hold, and both were the reported
// `gh pr create --web` and `wallfacer sync` corruption, which printed "^[[42;1R^[[42;1R" beside the prompt.
//
// The cause of both is that cm forwards the queries its own model answers to every client verbatim, on
// purpose, so a client's terminal sees questions cm never asked it and answers them anyway. termenv, which
// both of those programs use, sends OSC 11 and then CSI 6n behind it as a sentinel. cm proxies the OSC 11
// and answers the CSI 6n itself, and the client answers both.
//
// So, first, a reply has to answer the question that was asked: matching on the token alone accepted the
// terminal's cursor report as the background colour, wrote it to the pty, released cm's own cursor report
// from behind it, and discarded the real colour reply as unsolicited.
//
// And second, the chunk has to be split, because a terminal answers several questions in one write. Fixing
// only the matching left the doubling in place, measured in a real kitty: the client replied
// "OSC 11 colour" and "CSI 6n cursor report" concatenated, the colour matched the outstanding question, and
// the cursor report rode along in the same blob to the pty. Each sequence is therefore matched
// independently, and the ones answering nothing cm asked are dropped rather than carried by their
// neighbour.
func (s *Session) answerFromClient(tok *attachToken, data []byte) {
	// Split into individual sequences, since one write can carry answers to several questions. A chunk that
	// does not split cleanly is matched whole, which is what IsQueryReply already vouched for.
	replies := input.SplitReplies(data)
	if len(replies) == 0 {
		replies = [][]byte{data}
	}

	s.mu.Lock()
	var matched, unmatched int
	for _, reply := range replies {
		var found bool
		for _, r := range s.requests {
			if r.proxied && r.tok == tok && r.data == nil && query.AnswersQuery(r.seq, reply) {
				r.data = append([]byte(nil), reply...)
				r.proxied = false
				found = true
				matched++
				break
			}
		}
		if !found {
			unmatched++
		}
	}
	if matched == 0 {
		s.mu.Unlock()
		s.log.Debug("discarding an unsolicited reply from a client",
			"session", s.label, "bytes", len(data))
		return
	}
	ready := s.takeReadyLocked()
	s.mu.Unlock()

	if unmatched > 0 {
		// Worth a line: this is the shape of the reported bug, and it is otherwise invisible because the
		// dropped bytes are exactly the ones that used to appear beside the prompt.
		s.log.Debug("dropped replies answering nothing cm asked",
			"session", s.label, "count", unmatched)
	}
	s.writeReplies(ready)
}

// reofferQueries records questions a reconnecting client says it was asked and has not answered.
//
// The server's record of an outstanding question is in memory, and adoption resubscribes from where the old
// server stopped rather than re-reading the bytes that carried the query, so a restart forgets it and does not
// re-ask it. The reply then matches nothing, answerFromClient discards it, and the asking program gets no
// answer. The client is the only thing that still knows, so it re-offers and this records it again.
//
// Recorded as freshly asked, and that is a change to the answer budget rather than a repair, so it is worth
// being explicit. requestTimeout is 500ms and a restart takes seconds, so a question carried across one has
// already blown its budget: preserving the record without resetting the clock would change nothing, because
// sweepRequests would abandon it immediately. Measured both ways, and a reply arriving 2s late is discarded
// with no restart involved at all. So the budget now means "500ms from when this client was last asked", and
// the reply the terminal produced during the restart arrives within microseconds of the reconnect.
//
// This does not let a client answer for itself, which is the invariant that matters. cm is still the only
// writer of a reply to the pty, still matches a reply against a recorded question, and still asks one client
// at a time. What a client gains is having bytes it sends treated as a reply rather than as typing, and it can
// already send typing, which reaches the pty verbatim.
//
// A question re-offered that was in fact answered before the restart costs one requestTimeout of queue delay
// and then expires, which is the same outcome as a client that never answers. The client cannot tell the
// difference, because which question a reply settles is decided here against the query's bytes and never
// reported back.
//
// Called after attach rather than beside noteClientIdentity. The channel a question is asked on is created
// inside attach, so an earlier call found none and this dropped the re-offer by its own guard: the log said
// one question was re-offered and the reply still matched nothing, which reads as the wire being broken
// rather than as an ordering mistake.
func (s *Session) reofferQueries(tok *attachToken, queries [][]byte) {
	if tok == nil || len(queries) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.queries[tok] == nil {
		// No channel to ask on, so this client cannot be the answerer and recording a question against it
		// would hold the queue for nothing.
		return
	}
	for _, q := range queries {
		if len(q) == 0 {
			continue
		}
		s.requests = append(s.requests, &pendingRequest{
			proxied: true,
			tok:     tok,
			seq:     append([]byte(nil), q...),
			asked:   s.now(),
		})
	}
	s.log.Debug("re-registered questions a reconnecting client had not answered",
		"session", s.label, "count", len(queries))
}

// awaitingReply reports whether cm has asked this client a question it has not answered.
//
// Read by the input framer, which treats an incomplete sequence at the end of a read differently depending
// on the answer: held when a reply is expected, released as a keypress otherwise. That is what makes the
// holdback cost nothing in normal use, since the overwhelmingly common state is nothing outstanding.
//
// Only questions proxied to *this* client count. A question asked of another client cannot be answered by
// this one, so holding this one's keystrokes for it would be latency for nothing.
func (s *Session) awaitingReply(tok *attachToken) bool {
	if tok == nil {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, r := range s.requests {
		if r.proxied && r.tok == tok {
			return true
		}
	}
	return false
}

// queueOrWriteReply decides what to do with a reply cm's own emulator generated.
//
// Written immediately when nothing is outstanding, which is the overwhelmingly common case and keeps the
// fast path fast. Queued behind an outstanding question otherwise, so the program receives answers in the
// order it asked.
func (s *Session) queueOrWriteReply(replies [][]byte) {
	if len(replies) == 0 {
		return
	}

	s.mu.Lock()
	if len(s.requests) == 0 {
		s.mu.Unlock()
		s.writeReplies(replies)
		return
	}
	for _, data := range replies {
		s.requests = append(s.requests, &pendingRequest{
			data:  append([]byte(nil), data...),
			asked: s.now(),
		})
	}
	ready := s.takeReadyLocked()
	s.mu.Unlock()

	s.writeReplies(ready)
}

// takeReadyLocked removes and returns the replies that may now be written, in order.
//
// Stops at the first entry still waiting for a client, which is the ordering guarantee: everything behind
// an unanswered question stays queued. Callers must hold mu.
func (s *Session) takeReadyLocked() [][]byte {
	var out [][]byte
	i := 0
	for ; i < len(s.requests); i++ {
		r := s.requests[i]
		if r.proxied {
			// Still waiting on a client. Nothing behind this may go out yet.
			break
		}
		if len(r.data) > 0 {
			out = append(out, r.data)
		}
	}
	s.requests = s.requests[i:]
	return out
}

// sweepRequests expires questions a client never answered, releasing whatever queued behind them.
//
// The release is the point rather than the cleanup. An expired question means the asking program gets no
// answer, which is unfortunate and is also what happens today; leaving the entries behind it stuck would
// be far worse, because every later query on the session would be held by a question that can no longer be
// answered.
func (s *Session) sweepRequests() {
	s.mu.Lock()
	cutoff := s.now().Add(-requestTimeout)
	expired := 0
	for _, r := range s.requests {
		if r.proxied && r.asked.Before(cutoff) {
			// Treated as answered with nothing, so takeReadyLocked can move past it.
			r.proxied = false
			r.data = nil
			expired++
		}
	}
	ready := s.takeReadyLocked()
	s.mu.Unlock()

	if expired > 0 {
		s.log.Debug("expired proxied terminal queries with no reply",
			"session", s.label, "count", expired)
	}
	s.writeReplies(ready)
}

// writeReplies sends replies to the pty in order.
func (s *Session) writeReplies(replies [][]byte) {
	for _, data := range replies {
		if err := s.Write(context.Background(), data); err != nil {
			// A program that queried the terminal is now waiting for an answer it will never get, so this
			// explains an otherwise inexplicable hang.
			s.log.Warn("delivering terminal response to the pty failed",
				"session", s.label, "bytes", len(data), "error", err)
			return
		}
	}
}

// runRequestSweeper expires stale proxied queries for the life of the session.
func (s *Session) runRequestSweeper() {
	t := time.NewTicker(requestSweep)
	defer t.Stop()
	for {
		select {
		case <-s.done:
			return
		case <-t.C:
			s.sweepRequests()
		}
	}
}

// now reads the clock through a field so a test can control it.
//
// Timing is the hard part of this mechanism, and the alternative to an injectable clock is a test that
// sleeps past a 500ms timeout, which is slow and racy. See the tests for how the expiry is driven
// deterministically.
func (s *Session) now() time.Time {
	if s.clock != nil {
		return s.clock()
	}
	return time.Now()
}
