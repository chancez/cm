package server

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// A stream ttrpc gave up on is not the session ending, and treating it as one stranded a live shell.
//
// ttrpc v1.2.9 closes a stream whose consumer has not drained within a second; v1.2.7 blocked instead. The
// pump's consumer does real work per chunk -- the log append, the graphics transform, feeding the emulator
// -- so it can be that consumer. finish then asks the shim for an exit status, is told twenty times that
// the shell has not exited, and records exit code -1 with the log closed, while the shim and the shell
// carry on with nothing able to reach them.
//
// Driven at the pump rather than through a real ttrpc stream, because the state to construct is "the
// stream died at a known position" and racing a real one onto that position is what the internal/transport
// measurement is for.
func TestADroppedOutputStreamDoesNotEndALiveSession(t *testing.T) {
	first := newScriptedStream([]*shimv1.Output{{Seq: 0, Data: []byte("first")}}, ttrpc.ErrStreamFull)
	// Held open after its chunk, which is what a live shell that has stopped producing looks like. A stream
	// that ended here would end the session and the assertions below would be about teardown instead.
	second := newScriptedStream([]*shimv1.Output{{Seq: 5, Data: []byte("second")}}, nil)
	second.hold = true
	t.Cleanup(second.releaseNow)

	// Only the second: the first is handed to the pump directly, so every Subscribe recorded here is a
	// resubscribe.
	shim := &resubscribeShim{streams: []*scriptedStream{second}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess := pumpTestSession(t, shim, ctx, cancel)

	go sess.pump(first)

	// Read rather than polled: the log delivers to a subscriber, so this waits for the bytes instead of
	// racing them.
	const want = "firstsecond"
	r := sess.recent.Subscribe(0)
	defer r.Close()
	var got []byte
	for len(got) < len(want) {
		c, err := r.Next(ctx)
		if err != nil {
			t.Fatalf("reading the session log: %v, got %q so far, want %q", err, got, want)
		}
		got = append(got, c.Data...)
	}
	if string(got) != want {
		t.Errorf("session log = %q, want %q: the bytes after the dropped stream must still arrive",
			got, want)
	}

	// The position the resume asked for, which is where the first stream's bytes ended. Asking from zero
	// would replay the session from the start of what the shim retains, and asking from further on would
	// skip whatever the dropped stream was carrying.
	if from := shim.subscribedFrom(); !slices.Equal(from, []uint64{5}) {
		t.Errorf("subscribed from %v, want [5]", from)
	}

	if ended, code := sess.Ended(); ended || code != 0 {
		t.Errorf("Ended() = %v, %d, want false, 0: the shell is still running", ended, code)
	}
}

// A sequence split across the drop is not written twice.
//
// The subtle half of resuming from lastSeq: that position deliberately does not count bytes held back for
// an unfinished escape sequence, because the shim will send them again. So the held copy has to go, or the
// resume prepends those bytes to themselves. Here the first stream ends mid-CSI and the second carries the
// whole sequence from where the first left off.
func TestAResumeDoesNotReplayTheHeldTailTwice(t *testing.T) {
	first := newScriptedStream([]*shimv1.Output{
		{Seq: 0, Data: []byte("A\x1b[")},
	}, ttrpc.ErrStreamFull)
	second := newScriptedStream([]*shimv1.Output{
		{Seq: 1, Data: []byte("\x1b[31m")},
	}, nil)
	second.hold = true
	t.Cleanup(second.releaseNow)
	shim := &resubscribeShim{streams: []*scriptedStream{second}}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	sess := pumpTestSession(t, shim, ctx, cancel)

	go sess.pump(first)

	// "A" plus the sequence, once. Keeping the held tail produces "A\x1b[\x1b[31m", which is the same bytes
	// with a truncated sequence spliced in front of the real one.
	const want = "A\x1b[31m"
	r := sess.recent.Subscribe(0)
	defer r.Close()
	var got []byte
	for len(got) < len(want) {
		c, err := r.Next(ctx)
		if err != nil {
			t.Fatalf("reading the session log: %v, got %q so far, want %q", err, got, want)
		}
		got = append(got, c.Data...)
	}
	if string(got) != want {
		t.Errorf("session log = %q, want %q", got, want)
	}
	if from := shim.subscribedFrom(); !slices.Equal(from, []uint64{1}) {
		t.Errorf("subscribed from %v, want [1]: the held tail is not counted as consumed", from)
	}
}

// pumpTestSession is a Session with just enough wired up to run the pump.
func pumpTestSession(
	t *testing.T, shim shimv1.ShimClient, ctx context.Context, cancel context.CancelFunc,
) *Session {
	t.Helper()

	return &Session{
		id:          "streamfull",
		shim:        shim,
		conn:        noopConn{},
		recent:      seqlog.NewAt[seq.Log](DefaultRecentBytes, 0),
		gfxStore:    graphics.NewStore(0),
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		queries:     make(map[*attachToken]chan []byte),
		metaSubs:    make(map[*metaSub]struct{}),
		boundaries:  osc.NewBoundaryTracker(0),
		done:        make(chan struct{}),
		log:         slog.New(slog.DiscardHandler),
		pumpCtx:     ctx,
		stopPump:    cancel,
	}
}

// noopConn stands in for the connection finish closes.
type noopConn struct{}

func (noopConn) Close() error { return nil }

// scriptedStream is a Shim_SubscribeClient that delivers a fixed list of chunks and then ends.
type scriptedStream struct {
	mu     sync.Mutex
	chunks []*shimv1.Output
	// endWith is returned once the chunks run out. Nil means io.EOF, the clean end.
	endWith error
	// hold waits for release before ending, for a stream that should stay open.
	hold    bool
	release chan struct{}
	closed  sync.Once
}

func newScriptedStream(chunks []*shimv1.Output, endWith error) *scriptedStream {
	return &scriptedStream{chunks: chunks, endWith: endWith, release: make(chan struct{})}
}

func (s *scriptedStream) releaseNow() {
	s.closed.Do(func() { close(s.release) })
}

func (s *scriptedStream) Recv() (*shimv1.Output, error) {
	s.mu.Lock()
	if len(s.chunks) > 0 {
		out := s.chunks[0]
		s.chunks = s.chunks[1:]
		s.mu.Unlock()
		return out, nil
	}
	hold, end := s.hold, s.endWith
	s.mu.Unlock()

	if hold {
		<-s.release
	}
	if end == nil {
		return nil, io.EOF
	}
	return nil, end
}

func (s *scriptedStream) CloseSend() error  { return nil }
func (s *scriptedStream) SendMsg(any) error { return nil }
func (s *scriptedStream) RecvMsg(any) error { return nil }

// resubscribeShim hands out the scripted streams in order and reports a shell that is still running.
type resubscribeShim struct {
	mu      sync.Mutex
	streams []*scriptedStream
	// froms records the position each Subscribe asked for.
	froms []uint64
}

func (r *resubscribeShim) Subscribe(
	_ context.Context, req *shimv1.SubscribeRequest,
) (shimv1.Shim_SubscribeClient, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.froms = append(r.froms, req.FromSeq)
	if len(r.streams) == 0 {
		return nil, errors.New("no stream left")
	}
	s := r.streams[0]
	r.streams = r.streams[1:]
	return s, nil
}

func (r *resubscribeShim) subscribedFrom() []uint64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]uint64(nil), r.froms...)
}

// State reports a shell that is still running, which is the point: the pump must not conclude otherwise
// from a dropped stream.
func (r *resubscribeShim) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	return &shimv1.StateResponse{Exited: false}, nil
}

func (r *resubscribeShim) Write(context.Context, *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	return nil, errors.New("unused")
}

func (r *resubscribeShim) Resize(context.Context, *shimv1.ResizeRequest) (*shimv1.ResizeResponse, error) {
	return nil, errors.New("unused")
}

func (r *resubscribeShim) Signal(context.Context, *shimv1.SignalRequest) (*shimv1.SignalResponse, error) {
	return nil, errors.New("unused")
}

func (r *resubscribeShim) Shutdown(
	context.Context, *shimv1.ShutdownRequest,
) (*shimv1.ShutdownResponse, error) {
	return nil, errors.New("unused")
}
