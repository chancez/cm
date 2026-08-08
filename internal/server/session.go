// Package server owns session bookkeeping and is the only entry point clients use.
//
// It sits between clients and shims: it spawns and adopts shims, proxies terminal bytes,
// and holds whatever terminal state is needed to bring a reattaching client up to date.
// Clients never talk to a shim directly, which keeps fanout and policy in one place and
// leaves shims ignorant.
package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync"
	"sync/atomic"

	"github.com/containerd/ttrpc"

	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/store"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// ErrSessionGone reports that a session's shell has exited, so a client should stop rather
// than reconnect.
var ErrSessionGone = errors.New("session has ended")

// DefaultRecentBytes bounds the output the server retains per session.
//
// The server keeps a window of what it has consumed so a client attaching mid-session sees
// recent output rather than starting from a blank screen, and so a client that reconnects
// after a brief outage can resume from its own position. Once the terminal model lands, a
// fresh attach is served by replaying screen state instead, and this only has to cover the
// reconnect case.
const DefaultRecentBytes = 1 << 20 // 1 MiB

// Session is one live session as the server sees it: a connection to its shim, the
// terminal state derived from its output, and the set of attached clients.
type Session struct {
	name   string
	record store.Session

	// conn and shim reach the session's shim. The server is the shim's only client.
	conn *ttrpc.Client
	shim shimv1.ShimClient

	// term accumulates terminal state so a reattaching client can be restored. Nil until
	// the VT layer lands; the fanout below does not depend on it.
	term Terminal

	// recent holds output consumed from the shim, so clients can subscribe from a
	// position rather than only receiving what arrives after they connect. Using the same
	// log type as the shim means the gap semantics are identical on both hops.
	recent *seqlog.Log

	mu sync.Mutex
	// lastSeq is one past the last sequence number consumed from the shim, and is the
	// resume point if this server restarts.
	lastSeq uint64
	// ended is set once the shim reports the shell exited.
	ended    bool
	exitCode int

	// closed guards teardown, which both the pump ending and an explicit Close can reach.
	closeOnce sync.Once
	done      chan struct{}
	// stopPump ends the shim subscription, which is how Close stops consuming output.
	stopPump context.CancelFunc
	// releasing records that this server is letting go of a still-live session, so the
	// pump ending is not mistaken for the session ending.
	releasing atomic.Bool

	// clients counts attached clients for reporting.
	clients atomic.Int64
}

// Terminal is the terminal state the server maintains for a session.
//
// Declared as an interface so the fanout and reconnect logic can be built and tested
// before the libghostty binding exists, and so tests can substitute a trivial
// implementation instead of a real emulator.
type Terminal interface {
	// Write feeds terminal output to the emulator.
	Write(p []byte) error
	// Restore returns bytes that reproduce the current screen on a fresh terminal.
	Restore() ([]byte, error)
	// Resize changes the model's size so a restore matches the terminal showing it.
	Resize(rows, cols uint16) error
	// TakePending returns bytes the emulator generated that must reach the pty, such as
	// responses to device queries. A program that asks the terminal a question blocks until
	// answered, so these have to be delivered.
	TakePending() [][]byte
	// Title returns the last title the shell reported.
	Title() string
	// Pwd returns the last directory the shell reported, unparsed.
	Pwd() string
	// Close releases emulator resources.
	Close() error
}

// dialShim connects to a shim's socket.
func dialShim(socket string) (*ttrpc.Client, shimv1.ShimClient, error) {
	conn, err := net.Dial("unix", socket)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to shim at %s: %w", socket, err)
	}
	cl := ttrpc.NewClient(conn)
	return cl, shimv1.NewShimClient(cl), nil
}

// newSession connects to a shim and starts consuming its output from fromSeq.
//
// The output subscription deliberately does not use the caller's context. A session
// outlives the request that created it: the client that attached first will detach, and its
// context will be cancelled, but the session must keep consuming so later clients see
// current state. Its lifetime is instead bound to the session, ended by Close.
func newSession(rec store.Session, term Terminal, fromSeq uint64) (*Session, error) {
	conn, shim, err := dialShim(rec.ShimSocket)
	if err != nil {
		return nil, err
	}

	pumpCtx, stopPump := context.WithCancel(context.Background())

	s := &Session{
		name:     rec.Name,
		record:   rec,
		conn:     conn,
		shim:     shim,
		term:     term,
		recent:   seqlog.NewAt(DefaultRecentBytes, fromSeq),
		lastSeq:  fromSeq,
		done:     make(chan struct{}),
		stopPump: stopPump,
	}

	sub, err := shim.Subscribe(pumpCtx, &shimv1.SubscribeRequest{FromSeq: fromSeq})
	if err != nil {
		stopPump()
		conn.Close()
		return nil, fmt.Errorf("subscribing to shim for %s: %w", rec.Name, err)
	}

	go s.pump(sub)
	return s, nil
}

// pump consumes the shim's output stream, feeds the terminal model, and fans out to
// clients.
//
// This is the only writer to terminal state, so the emulator needs no locking of its own.
func (s *Session) pump(sub shimv1.Shim_SubscribeClient) {
	defer s.finish()

	for {
		out, err := sub.Recv()
		if err != nil {
			// The stream ends when the shell exits or the shim goes away. Either way this
			// session is over; which one is recorded by finish via a State call.
			return
		}

		if s.term != nil {
			if err := s.term.Write(out.Data); err != nil {
				// A terminal model that cannot keep up would make restores wrong, but
				// dropping the session over it would be worse: live output still works.
				// Give up on restores instead by discarding the model.
				s.mu.Lock()
				s.term = nil
				s.mu.Unlock()
			} else {
				s.drainPending()
			}
		}

		// Appending both records the output for later subscribers and wakes current ones.
		// A slow client cannot stall the session: it simply falls behind and is told there
		// is a gap if the window passes it.
		s.recent.Append(out.Data)

		s.mu.Lock()
		s.lastSeq = out.Seq + uint64(len(out.Data))
		s.mu.Unlock()
	}
}

// finish records the session's outcome and releases subscribers.
//
// Skipped entirely when the server is merely letting go of a live session: the shim is still
// holding a running shell, and recording an outcome would tell the next server the session is
// over when it is not.
func (s *Session) finish() {
	if s.releasing.Load() {
		s.closeOnce.Do(func() { close(s.done) })
		return
	}

	// Ask the shim why the stream ended. A shell that exited has a status worth
	// reporting; an unreachable shim means the outcome is unknown.
	code, exited := 0, false
	if st, err := s.shim.State(context.Background(), &shimv1.StateRequest{}); err == nil {
		exited, code = st.Exited, int(st.ExitCode)
	}

	s.mu.Lock()
	s.ended = true
	s.exitCode = code
	if !exited {
		// Distinguish "shell exited" from "shim vanished" for the caller, using a code
		// that cannot collide with a real exit status.
		s.exitCode = -1
	}
	if s.term != nil {
		s.term.Close()
		s.term = nil
	}
	s.mu.Unlock()

	// Closing releases subscribers once they have drained remaining output, so a client
	// still sees a shell's final bytes before being told the session ended.
	s.recent.Close()

	s.closeOnce.Do(func() { close(s.done) })
	s.stopPump()
	s.conn.Close()
}

// Close stops consuming the shim's output and releases the connection, without terminating
// the shell. Used when a server shuts down: the shim keeps running so the next server can
// adopt it.
//
// Marking the session as being released first is what keeps the pump ending from being
// recorded as the session ending. Without it, a normal server shutdown would mark every live
// session dead and the next server would refuse to adopt them.
func (s *Session) Close() {
	s.releasing.Store(true)
	s.stopPump()
	s.conn.Close()
}

// Releasing reports whether this session is being let go while still alive, rather than
// having ended.
func (s *Session) Releasing() bool { return s.releasing.Load() }

// drainPending sends bytes the emulator generated back to the pty.
//
// These are answers to questions a program asked the terminal, such as a device status report.
// Without this the program waits for a reply that never comes; zmx works around the same
// problem by answering DA1 queries by hand.
func (s *Session) drainPending() {
	if s.term == nil {
		return
	}
	for _, data := range s.term.TakePending() {
		if err := s.Write(context.Background(), data); err != nil {
			return
		}
	}
}

// Done is closed when the session ends.
func (s *Session) Done() <-chan struct{} { return s.done }

// Ended reports whether the session is over and, if so, its exit code. A code of -1 means
// the shim became unreachable rather than the shell exiting.
func (s *Session) Ended() (bool, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.ended, s.exitCode
}

// LastSeq returns the resume point: one past the last byte consumed from the shim.
func (s *Session) LastSeq() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

// Clients reports how many clients are attached.
func (s *Session) Clients() int64 { return s.clients.Load() }

// attach registers a subscriber and returns it along with the sequence number its stream
// begins at.
//
// The caller gets restore bytes and a starting sequence under one lock, so no output can
// slip between snapshotting the screen and subscribing. Getting that wrong would show a
// client a screen that is either missing bytes or replaying them twice.
func (s *Session) attach(resumeFrom *uint64) (*seqlog.Reader, []byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ended {
		return nil, nil, ErrSessionGone
	}

	// A resuming client already has the session on screen and wants only the bytes it
	// missed. A fresh client needs the screen rebuilt: from serialized terminal state when
	// there is a model, and otherwise from whatever recent output is retained, which is
	// worse but still better than a blank screen.
	from := s.recent.Oldest()
	var restore []byte
	if resumeFrom != nil {
		from = *resumeFrom
	} else if s.term != nil {
		b, err := s.term.Restore()
		if err != nil {
			return nil, nil, fmt.Errorf("serializing terminal state: %w", err)
		}
		restore = b
		// State is replayed, so streaming starts at the present rather than repeating
		// history the snapshot already covers.
		from = s.lastSeq
	}

	r := s.recent.Subscribe(from)
	s.clients.Add(1)
	return r, restore, nil
}

// detach releases a subscriber.
func (s *Session) detach(r *seqlog.Reader) {
	r.Close()
	s.clients.Add(-1)
}

// Write sends input to the session's shell.
func (s *Session) Write(ctx context.Context, data []byte) error {
	_, err := s.shim.Write(ctx, &shimv1.WriteRequest{Data: data})
	return err
}

// Resize sets the session's window size, on the pty and on the terminal model.
//
// The model must track the pty or a restore would describe a screen of the wrong shape.
func (s *Session) Resize(ctx context.Context, rows, cols, xpixel, ypixel uint32) error {
	if _, err := s.shim.Resize(ctx, &shimv1.ResizeRequest{
		Rows: rows, Cols: cols, XPixel: xpixel, YPixel: ypixel,
	}); err != nil {
		return err
	}

	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term != nil && rows > 0 && cols > 0 {
		if err := term.Resize(uint16(rows), uint16(cols)); err != nil {
			return fmt.Errorf("resizing terminal model: %w", err)
		}
	}
	return nil
}

// Title returns the session's title as reported by the shell.
func (s *Session) Title() string {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return ""
	}
	return term.Title()
}

// Pwd returns the session's directory as reported by the shell, unparsed.
func (s *Session) Pwd() string {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return ""
	}
	return term.Pwd()
}

// Shutdown terminates the session's shell and its shim.
func (s *Session) Shutdown(ctx context.Context, force bool) error {
	_, err := s.shim.Shutdown(ctx, &shimv1.ShutdownRequest{Force: force})
	return err
}

// State queries the shim directly, which is the authority on whether a session is alive.
func (s *Session) State(ctx context.Context) (*shimv1.StateResponse, error) {
	return s.shim.State(ctx, &shimv1.StateRequest{})
}
