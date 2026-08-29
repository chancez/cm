package server

import (
	"testing"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
)

// TestNoteQueriesAtEveryChunkBoundary sweeps a pty read boundary through a terminal-only query.
//
// noteQueries scans each chunk of session output for questions cm cannot answer, and records them so the
// reply a client sends back can be matched to the question that asked for it. It is stateless, and its own
// comment says what it does with a fragment: "Not a sequence, or an incomplete one at the end of the chunk.
// Advance a byte."
//
// So a query split by a read boundary is never registered, and the consequence is not that the query is
// lost. The stream is forwarded verbatim, deliberately, so the client's terminal still receives the question
// and still answers it. The reply comes back, answerFromClient finds no outstanding request, and discards
// it: `if matched == 0 { ... return }`. The program that asked waits for an answer that cm threw away.
//
// That is a hang, and it is the hang this proxy exists to fix. `wallfacer -h` blocking on OSC 11 is the
// recorded case, which is why OSC 11 is the first fixture here.
//
// Counted rather than inspected, because the question is only "was it registered", and a session built as a
// literal gives an exact answer where an end-to-end run would have to race the boundary.
func TestNoteQueriesAtEveryChunkBoundary(t *testing.T) {
	for _, tc := range []struct {
		name  string
		query string
	}{
		// The recorded hang. wallfacer -h blocks on this one.
		//
		// OSC 4 is deliberately not here: libghostty answers a palette query from the palette it models, so
		// it is in the answerable set rather than the proxied one. classifyOSCQuery says so and the summary
		// comment above it used to disagree.
		{name: "OSC 11 background colour", query: "\x1b]11;?\x07"},
		{name: "OSC 11 ST terminated", query: "\x1b]11;?\x1b\\"},
		{name: "OSC 10 foreground colour", query: "\x1b]10;?\x07"},
		{name: "OSC 52 clipboard read", query: "\x1b]52;c;?\x07"},
		{name: "CSI 14t pixel size", query: "\x1b[14t"},
		{name: "CSI 16t cell size", query: "\x1b[16t"},
		{name: "XTGETTCAP", query: "\x1bP+q544e\x1b\\"},
		{name: "kitty graphics query", query: "\x1b_Ga=q,i=1,s=1,v=1,t=d,f=24;AAAA\x1b\\"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Surrounded by ordinary output, which is how a query really arrives: a program paints and asks
			// in the same write, and the pty splits that write wherever the buffer ended.
			stream := "painting the screen" + tc.query + "and carrying on"
			start := len("painting the screen")
			end := start + len(tc.query)

			// The control: fed whole, the query is registered. Without this a fixture cm does not classify
			// as terminal-only would report zero at every split and look like a bug everywhere.
			if got := countProxied(t, stream); got != 1 {
				t.Fatalf("fed whole, %d queries were registered, want 1: this fixture is not one cm proxies, "+
					"so the sweep below would prove nothing", got)
			}

			var lost []int
			for split := 1; split < len(stream); split++ {
				if countProxiedSplit(t, stream, split) != 1 {
					lost = append(lost, split)
				}
			}

			if len(lost) > 0 {
				t.Errorf("a read boundary at %v loses the query, so the client answers a question cm never "+
					"recorded and the reply is discarded as unsolicited. The program that asked waits "+
					"forever.\nquery occupies bytes %d..%d of %d", lost, start, end, len(stream))
			}
		})
	}
}

// countProxied feeds a stream to a session in one chunk and reports how many queries were proxied.
func countProxied(t *testing.T, stream string) int {
	t.Helper()
	sess, att := sessionWithClient(t)
	defer sess.detach(att)
	sess.processChunk([]byte(stream), 0)
	return outstandingRequests(sess)
}

// countProxiedSplit feeds the same stream as two chunks, split at the given offset.
func countProxiedSplit(t *testing.T, stream string, split int) int {
	t.Helper()
	sess, att := sessionWithClient(t)
	defer sess.detach(att)
	// Positions advance the way the shim numbers them: the second chunk starts where the first ended,
	// whatever the pump chose to consume.
	sess.processChunk([]byte(stream[:split]), 0)
	sess.processChunk([]byte(stream[split:]), seq.Shim(split))
	return outstandingRequests(sess)
}

// sessionWithClient builds a session with one attached client, since a query is only proxied when there is
// somebody to ask.
func sessionWithClient(t *testing.T) (*Session, attachment) {
	t.Helper()
	sess := &Session{
		id:          "queries",
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		term:        &fakeTerminal{restore: []byte("SCREEN")},
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		queries:     make(map[*attachToken]chan []byte),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
	}
	att, err := sess.attach(nil, nil)
	if err != nil {
		t.Fatalf("attach() error = %v", err)
	}
	return sess, att
}

func outstandingRequests(s *Session) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := 0
	for _, r := range s.requests {
		if r.proxied {
			n++
		}
	}
	return n
}

// TestALargePartialSequenceIsNotHeld checks the bound, which is what keeps the holdback out of the way of
// kitty graphics.
//
// A graphics transmission is an APC carrying a payload chunked at about 4 KiB, so a partial one is routinely
// larger than any query or prompt marker. Holding those would delay every image and buffer megabytes, and it
// would buy nothing, because the graphics scanner already reassembles a transmission across chunks. So past
// maxHeldTail the tail is passed through, which is what happened before any holdback existed.
//
// Asserted on the log rather than on the field, since what matters is that clients received the bytes.
func TestALargePartialSequenceIsNotHeld(t *testing.T) {
	sess, att := sessionWithClient(t)
	defer sess.detach(att)

	// An APC that is still arriving, longer than the bound.
	chunk := append([]byte("\x1b_Ga=t,f=100;"), make([]byte, maxHeldTail+64)...)
	for i := range chunk[13:] {
		chunk[13+i] = 'A'
	}
	sess.processChunk(chunk, 0)

	if held := len(sess.outPartial); held != 0 {
		t.Errorf("held %d bytes of a %d-byte partial APC, want 0: holding a graphics payload delays every "+
			"image, and the graphics scanner already reassembles one across chunks", held, len(chunk))
	}
	if got := sess.recent.Next(); got != seq.Log(len(chunk)) {
		t.Errorf("the log advanced to %d after a %d-byte chunk, want all of it: bytes withheld past the "+
			"bound never reach a client", got, len(chunk))
	}
}

// TestAShortPartialSequenceIsHeld is the other side of the bound, so the two together say where it sits.
func TestAShortPartialSequenceIsHeld(t *testing.T) {
	sess, att := sessionWithClient(t)
	defer sess.detach(att)

	// A query that has not finished arriving.
	sess.processChunk([]byte("output\x1b]11;?"), 0)

	if held := len(sess.outPartial); held != 6 {
		t.Errorf("held %d bytes of a split OSC 11 query, want 6: an unheld one is never recorded, so the "+
			"client answers a question cm did not ask and the reply is discarded", held)
	}
	if got := sess.recent.Next(); got != seq.Log(len("output")) {
		t.Errorf("the log advanced to %d, want %d: the held bytes must not reach a client until the "+
			"sequence is whole", got, len("output"))
	}

	// And the rest arriving releases it, whole.
	sess.processChunk([]byte("\x07rest"), seq.Shim(len("output\x1b]11;?")))
	if held := len(sess.outPartial); held != 0 {
		t.Errorf("still holding %d bytes after the sequence completed", held)
	}
	if got := sess.recent.Next(); got != seq.Log(len("output\x1b]11;?\x07rest")) {
		t.Errorf("the log advanced to %d, want the whole stream at %d",
			got, len("output\x1b]11;?\x07rest"))
	}
}
