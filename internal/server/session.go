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
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/transport"
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
	//
	// The interface rather than the concrete client, since the only thing done with it is closing: that
	// is what keeps the transport swappable without every holder naming it.
	conn transport.Conn
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
	// title and cwd are what the shell last reported about itself, for clients and for
	// listing. rawPwd is kept so a repeat report can be recognized without re-parsing.
	title  string
	rawPwd string
	cwd    osc.Cwd
	// command is what the shell last reported about itself via OSC 133: whether a command is running
	// and, when the shell says so, which one.
	//
	// Derived from the output stream rather than asked of the terminal model, because these are events
	// rather than state libghostty retains. A terminal has no "is a command running" to query.
	command osc.CommandState

	// reported is what a program inside the session said about itself, and takes precedence over
	// command above.
	//
	// Precedence, not merging: a program that says it is blocked knows something the shell cannot
	// express, since a shell reports a command as running whether it is computing or waiting at a prompt
	// of its own. Empty when nothing has reported, in which case the derived state stands.
	reported Reported

	// commands parses the OSC 133 markers out of the output stream.
	//
	// Outside mu deliberately: it is fed only by the pump, which is the single writer to terminal
	// state, so it needs no lock, and taking one per chunk of output would be on the hot path. The
	// state it produces is copied into command above, which is guarded.
	commands osc.CommandTracker

	// log records what this session does. Never nil.
	log *slog.Logger

	// resizePolicy decides which client sets the size. Empty behaves as ResizeLeader.
	resizePolicy ResizePolicy
	// clients tracks each attached client's size, keyed by its attachment.
	clientSizes map[*attachToken]*clientSize
	// leader is the attachment that last sent real typing, under ResizeLeader.
	leader *attachToken
	// attachOrder increases with each attach.
	attachOrder uint64

	// restored holds a screen replayed from a previous incarnation's saved log, handed to the first
	// client that attaches and then discarded.
	//
	// Discarded after one use because it describes a session that has since started producing its
	// own output; replaying it to a later client would show that client a screen from before the
	// reboot.
	restored []byte

	// metaSubs are notified when the title or directory changes: the manager persists them, and
	// each attached client forwards them on so a terminal emulator can react. Keyed by pointer
	// so a client can remove its own.
	metaSubs map[*metaSub]struct{}

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

// ResizePolicy decides which of several attached clients sets the session's size.
//
// It only matters with more than one client, which for per-window sessions is unusual but happens
// deliberately: attaching twice to compare, or following a session to watch it.
type ResizePolicy string

const (
	// ResizeLeader gives sizing to the client that last typed. Mouse motion, focus changes, and
	// replies to terminal queries do not count, so a window nobody is using cannot take over.
	ResizeLeader ResizePolicy = "leader"
	// ResizeLastAttach gives sizing to the most recently attached client.
	ResizeLastAttach ResizePolicy = "last-attach"
	// ResizeFirstAttach keeps sizing with the earliest attached client until it leaves.
	ResizeFirstAttach ResizePolicy = "first-attach"
	// ResizeSmallest sizes the pty to fit every client, so nothing is cut off for anyone at the cost
	// of nobody using their full window.
	ResizeSmallest ResizePolicy = "smallest"
)

// clientSize is one attached client's reported size.
type clientSize struct {
	rows, cols     uint16
	xpixel, ypixel uint16
	// order increases with each attach, so first and last can be identified.
	order uint64
	// readOnly clients never own sizing, since a follower reflowing the window it watches is the
	// bug this whole policy exists to avoid.
	readOnly bool
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
	// FocusReporting reports whether the program asked to be told about focus changes.
	FocusReporting() bool
	// Plain, VT, and HTML render the terminal contents, scrollback included, for a history
	// dump.
	Plain() ([]byte, error)
	// Tail renders the last lines as plain text, optionally rejoining soft-wrapped lines. A lines
	// value of zero means everything.
	Tail(lines int, unwrap bool) ([]byte, error)
	VT() ([]byte, error)
	HTML() ([]byte, error)
	// Close releases emulator resources.
	Close() error
}

// dialShim connects to a shim's socket.
func dialShim(socket string) (transport.Conn, shimv1.ShimClient, error) {
	conn, cl, err := transport.DialShim(socket)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to shim at %s: %w", socket, err)
	}
	return conn, cl, nil
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
		name:        rec.Name,
		record:      rec,
		conn:        conn,
		shim:        shim,
		term:        term,
		recent:      seqlog.NewAt(DefaultRecentBytes, fromSeq),
		metaSubs:    make(map[*metaSub]struct{}),
		log:         cmlog.Discard(),
		clientSizes: make(map[*attachToken]*clientSize),
		lastSeq:     fromSeq,
		done:        make(chan struct{}),
		stopPump:    stopPump,
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
				s.log.Error("terminal model failed, screen restore disabled for this session",
					"session", s.name, "error", err)
				s.mu.Lock()
				s.term = nil
				s.mu.Unlock()
			} else {
				s.drainPending()
				s.noteMetadata()
			}
		}

		// Track what the shell says about itself before the rewrite below, so the markers are read
		// exactly as the shell sent them.
		//
		// This is where "is a command running" comes from. The shell reports it via OSC 133 as part of
		// its normal output, so cm reads it in passing rather than asking the shell to maintain it: zmx
		// needed shell hooks writing a label for the same information.
		if s.commands.Feed(out.Data) {
			s.noteCommand()
		}

		// Forcing redraw=0 into prompt markers before anything else sees them. A multiplexer
		// sits between the shell and the outer terminal, so a terminal that trusts the shell to
		// repaint its prompt clears it and never gets a usable repaint back.
		data := osc.RewritePromptRedraw(out.Data)

		// Appending both records the output for later subscribers and wakes current ones.
		// A slow client cannot stall the session: it simply falls behind and is told there
		// is a gap if the window passes it.
		s.recent.Append(data)

		// Two sequence numbers, deliberately, because the rewrite above changes the length.
		//
		// lastSeq tracks the shim's numbering, since it is the position to resubscribe from after
		// a restart, and the shim knows nothing about the rewrite. Clients are served from
		// s.recent, which numbers the rewritten bytes.
		//
		// Conflating them desynchronizes the two by however much the rewrite added, which puts a
		// client's resume position inside an escape sequence and slices the ESC off the front of
		// it. The visible result is a cursor move rendering as literal text beside the prompt.
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

	// Ask the shim why the stream ended. A shell that exited has a status worth reporting; an
	// unreachable shim means the outcome is unknown.
	//
	// Retried briefly, because the shim exits once its shell does and the output stream closes at the
	// same moment. Asking once loses the race often enough that every normally-exiting session was
	// recorded as "dead" with no status, which is both wrong and indistinguishable from a shim that
	// really did vanish.
	code, exited := 0, false
	for attempt := range 20 {
		st, err := s.shim.State(context.Background(), &shimv1.StateRequest{})
		if err == nil {
			exited, code = st.Exited, int(st.ExitCode)
			if exited {
				break
			}
			// Reachable but not yet reporting an exit, so the shell is still being reaped.
		} else if attempt > 0 && !exited {
			// Gone before answering. Nothing more will be learned by asking again.
			break
		}
		time.Sleep(10 * time.Millisecond)
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
			// A program that queried the terminal is now waiting for an answer it will never get, so
			// this explains an otherwise inexplicable hang.
			s.log.Warn("delivering terminal response to the pty failed",
				"session", s.name, "bytes", len(data), "error", err)
			return
		}
	}
}

// noteMetadata records title and directory changes the shell reported.
//
// Reported values are decoded here rather than stored raw, because a client acting on them needs
// a usable path: OSC 7 sends a percent-encoded URI, and a session that has ssh'd elsewhere
// reports a directory that does not exist locally.
func (s *Session) noteMetadata() {
	if s.term == nil {
		return
	}
	title := s.term.Title()
	rawPwd := s.term.Pwd()

	s.mu.Lock()
	titleChanged := title != s.title
	pwdChanged := rawPwd != s.rawPwd
	if titleChanged {
		s.title = title
	}
	if pwdChanged {
		s.rawPwd = rawPwd
		if cwd, ok := osc.ParseCwd(rawPwd); ok {
			s.cwd = cwd
		} else {
			s.cwd = osc.Cwd{}
		}
	}
	cwd := s.cwd
	command := s.command
	reported := s.reported
	s.mu.Unlock()

	if !titleChanged && !pwdChanged {
		return
	}
	// The command state is included even though this call is about the title and directory: a
	// Metadata is a snapshot of everything a session reports, so omitting it here would publish a
	// zero value and make a client think nothing was running.
	s.publishMetadata(Metadata{Title: title, Cwd: cwd, Command: command, Reported: reported})
}

// noteCommand records a change in what the shell reported running, and tells clients.
//
// Published like title and cwd, because a terminal emulator wants the same kind of reaction to it: a
// tab can show what is running, and a close confirmation needs to know whether anything is.
func (s *Session) noteCommand() {
	state := s.commands.State()

	s.mu.Lock()
	if state == s.command {
		s.mu.Unlock()
		return
	}
	s.command = state
	title, cwd, reported := s.title, s.cwd, s.reported
	s.mu.Unlock()

	s.publishMetadata(Metadata{Title: title, Cwd: cwd, Command: state, Reported: reported})
}

// Command reports what the shell last said it was running.
func (s *Session) Command() osc.CommandState {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.command
}

// setReported records what a program in the session said about itself, and tells clients.
//
// Published like the derived state, because a terminal emulator or a waiting script reacts to it the same
// way. A zero value clears the report, so the session falls back to what cm derives.
func (s *Session) setReported(r Reported) {
	s.mu.Lock()
	if r == s.reported {
		s.mu.Unlock()
		return
	}
	s.reported = r
	title, cwd, cmd := s.title, s.cwd, s.command
	s.mu.Unlock()

	s.publishMetadata(Metadata{Title: title, Cwd: cwd, Command: cmd, Reported: r})
}

// Reported returns what a program in the session last said about itself.
func (s *Session) Reported() Reported {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reported
}

// CommandRuns counts the commands the shell has reported starting.
//
// Exists so a waiter can tell "a command ran and finished" from "nothing has happened yet", which the
// current state alone cannot express: both look idle. Counted by the tracker, which sees every marker,
// because a command fast enough for its start and end to arrive in one chunk of output is never
// observably running.
func (s *Session) CommandRuns() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.command.Runs
}

// Metadata is what a session reports about itself.
type Metadata struct {
	Title string
	Cwd   osc.Cwd
	// Command is what the shell reported running, from OSC 133.
	Command osc.CommandState
	// Reported is what a program inside the session said about itself, which takes precedence over
	// Command when set.
	Reported Reported
}

// metaSub receives metadata changes.
//
// Buffered with a depth of one and coalescing: a client only ever needs the latest values, so a
// slow reader should see the newest rather than a backlog, and must never stall the output pump.
type metaSub struct {
	ch chan Metadata
}

// publishMetadata delivers a change to every subscriber, replacing any undelivered value.
func (s *Session) publishMetadata(m Metadata) {
	s.mu.Lock()
	subs := make([]*metaSub, 0, len(s.metaSubs))
	for sub := range s.metaSubs {
		subs = append(subs, sub)
	}
	s.mu.Unlock()

	for _, sub := range subs {
		// Drop a stale pending value before sending, so the receiver gets current state rather
		// than history it no longer cares about.
		select {
		case <-sub.ch:
		default:
		}
		select {
		case sub.ch <- m:
		default:
		}
	}
}

// subscribeMetadata registers for metadata changes and delivers current values immediately, so a
// newly attached client does not have to wait for the shell to report again.
func (s *Session) subscribeMetadata() *metaSub {
	sub := &metaSub{ch: make(chan Metadata, 1)}

	s.mu.Lock()
	s.metaSubs[sub] = struct{}{}
	current := Metadata{Title: s.title, Cwd: s.cwd, Command: s.command, Reported: s.reported}
	s.mu.Unlock()

	// Command is part of the seed, and the condition below accounts for it. A subscriber that arrives
	// while a command is already running would otherwise be told nothing about it until the shell
	// reported again, which for a long build is the whole time it matters.
	if current.Title != "" || current.Cwd.Path != "" || current.Command.Running ||
		current.Reported.State != "" {
		sub.ch <- current
	}
	return sub
}

// unsubscribeMetadata removes a subscriber.
func (s *Session) unsubscribeMetadata(sub *metaSub) {
	s.mu.Lock()
	delete(s.metaSubs, sub)
	s.mu.Unlock()
}

// Metadata returns the session's title and decoded directory.
func (s *Session) Metadata() (title string, cwd osc.Cwd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.title, s.cwd
}

// CwdURI returns the directory exactly as the shell reported it, which for OSC 7 is a URI that
// keeps the host.
//
// Worth exposing alongside the decoded path: the host is what distinguishes a session that has
// ssh'd elsewhere, and a caller shown only a decoded path cannot tell.
func (s *Session) CwdURI() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.rawPwd
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

// attachToken identifies one attachment, so sizes and leadership can be tracked per client without
// the client having an identity of its own.
//
// The field is not decoration. Go may give every allocation of a zero-size type the same address, so
// an empty struct here made all tokens compare equal: the map held one entry no matter how many
// clients attached, and the first client owned sizing forever. Carrying the attach order gives each
// token a distinct address and doubles as useful identity in logs.
type attachToken struct {
	order uint64
}

// attachment is what a client gets from attaching.
type attachment struct {
	// token identifies this attachment for sizing purposes.
	token *attachToken
	// reader streams session output.
	reader *seqlog.Reader
	// restore holds bytes reproducing the current screen, empty when resuming.
	restore []byte
	// first reports that this is the only attached client, so a program that tracks focus should
	// be told someone is watching again.
	first bool
}

// attach registers a subscriber and returns it along with the sequence number its stream
// begins at.
//
// The caller gets restore bytes and a starting sequence under one lock, so no output can
// slip between snapshotting the screen and subscribing. Getting that wrong would show a
// client a screen that is either missing bytes or replaying them twice.
func (s *Session) attach(resumeFrom *uint64) (attachment, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.ended {
		return attachment{}, ErrSessionGone
	}

	// A resuming client already has the session on screen and wants only the bytes it
	// missed. A fresh client needs the screen rebuilt: from serialized terminal state when
	// there is a model, and otherwise from whatever recent output is retained, which is
	// worse but still better than a blank screen.
	from := s.recent.Oldest()
	var restore []byte

	// A screen replayed from a previous incarnation takes precedence, since the live model holds
	// only what this session has produced since it started, which is nothing yet.
	if resumeFrom == nil && len(s.restored) > 0 {
		restore = s.restored
		s.restored = nil
		return s.newAttachmentLocked(s.recent.Next(), restore), nil
	}

	if resumeFrom != nil {
		from = *resumeFrom
	} else if s.term != nil {
		b, err := s.term.Restore()
		if err != nil {
			return attachment{}, fmt.Errorf("serializing terminal state: %w", err)
		}
		restore = b
		// State is replayed, so streaming starts at the present rather than repeating history the
		// snapshot already covers.
		//
		// The log's own end, not lastSeq. lastSeq counts the shim's bytes, while the log numbers
		// the rewritten ones, and prompt rewriting makes those differ by however much it added.
		// Using lastSeq here starts the stream at an offset inside an escape sequence, which
		// slices the ESC off and leaves a cursor move rendering as literal text beside the prompt.
		from = s.recent.Next()
	}

	return s.newAttachmentLocked(from, restore), nil
}

// newAttachmentLocked builds an attachment and registers it for sizing.
//
// One place rather than at each return, because there are two paths out of attach and the earlier
// version registered on only one of them, so a client restored from disk had no size entry and could
// never own sizing.
func (s *Session) newAttachmentLocked(from uint64, restore []byte) attachment {
	s.attachOrder++
	token := &attachToken{order: s.attachOrder}
	s.clientSizes[token] = &clientSize{order: s.attachOrder}

	return attachment{
		token:   token,
		reader:  s.recent.Subscribe(from),
		restore: restore,
		first:   s.clients.Add(1) == 1,
	}
}

// registerClientSize records what one client says its terminal is, and reports whether the session
// should now resize to it.
//
// Returning a decision rather than resizing directly keeps the policy in one place and leaves the
// RPC layer to do the call, which is where the context and error handling belong.
func (s *Session) registerClientSize(
	tok *attachToken, rows, cols, xpixel, ypixel uint16, readOnly bool,
) (wantRows, wantCols, wantX, wantY uint16, resize bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	cs := s.clientSizes[tok]
	if cs == nil {
		// Detached already, so it has no say.
		return 0, 0, 0, 0, false
	}
	cs.rows, cs.cols, cs.xpixel, cs.ypixel = rows, cols, xpixel, ypixel
	cs.readOnly = readOnly

	if readOnly || rows == 0 || cols == 0 {
		return 0, 0, 0, 0, false
	}

	policy := s.resizePolicy
	if policy == "" {
		policy = ResizeLeader
	}

	switch policy {
	case ResizeLastAttach:
		// The newest attach wins, which is what a single-client setup wants and what cm did before
		// this was configurable.
		return rows, cols, xpixel, ypixel, true

	case ResizeFirstAttach:
		// Only the earliest remaining client sizes the session.
		if s.earliestLocked() != tok {
			return 0, 0, 0, 0, false
		}
		return rows, cols, xpixel, ypixel, true

	case ResizeSmallest:
		r, c, ok := s.smallestLocked()
		return r, c, xpixel, ypixel, ok

	default: // ResizeLeader
		// An attaching client claims sizing only when nothing else holds it. Otherwise the window
		// someone is working in keeps its size until they type somewhere else.
		if s.leader == nil || s.clientSizes[s.leader] == nil {
			s.leader = tok
			return rows, cols, xpixel, ypixel, true
		}
		if s.leader != tok {
			return 0, 0, 0, 0, false
		}
		return rows, cols, xpixel, ypixel, true
	}
}

// claimLeadership records that a client typed, and reports its size when leadership moved.
//
// Only meaningful under ResizeLeader; the other policies ignore typing entirely.
func (s *Session) claimLeadership(tok *attachToken) (rows, cols, xpixel, ypixel uint16, resize bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	policy := s.resizePolicy
	if policy == "" {
		policy = ResizeLeader
	}
	if policy != ResizeLeader {
		return 0, 0, 0, 0, false
	}

	cs := s.clientSizes[tok]
	if cs == nil || cs.readOnly {
		// A follower never becomes leader, whatever it sends.
		return 0, 0, 0, 0, false
	}
	if s.leader == tok {
		return 0, 0, 0, 0, false
	}

	s.leader = tok
	if cs.rows == 0 || cs.cols == 0 {
		// Typed before reporting a size, so there is nothing to resize to yet.
		return 0, 0, 0, 0, false
	}
	return cs.rows, cs.cols, cs.xpixel, cs.ypixel, true
}

// earliestLocked returns the attachment with the lowest order that can own sizing.
func (s *Session) earliestLocked() *attachToken {
	var (
		best  *attachToken
		order uint64
	)
	for tok, cs := range s.clientSizes {
		if cs.readOnly {
			continue
		}
		if best == nil || cs.order < order {
			best, order = tok, cs.order
		}
	}
	return best
}

// smallestLocked returns the largest size that fits every client that has reported one.
func (s *Session) smallestLocked() (rows, cols uint16, ok bool) {
	for _, cs := range s.clientSizes {
		if cs.readOnly || cs.rows == 0 || cs.cols == 0 {
			continue
		}
		if !ok {
			rows, cols, ok = cs.rows, cs.cols, true
			continue
		}
		rows = min(rows, cs.rows)
		cols = min(cols, cs.cols)
	}
	return rows, cols, ok
}

// releaseClientSize forgets a detached client, and reports a size to fall back to when its departure
// changes who owns sizing.
func (s *Session) releaseClientSize(tok *attachToken) (rows, cols, xpixel, ypixel uint16, resize bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	delete(s.clientSizes, tok)
	wasLeader := s.leader == tok
	if wasLeader {
		// Unclaimed rather than transferred. The session keeps its current size until someone types,
		// because reflowing a window nobody touched is exactly the surprise this avoids.
		s.leader = nil
	}

	policy := s.resizePolicy
	if policy == "" {
		policy = ResizeLeader
	}

	switch policy {
	case ResizeSmallest:
		// One fewer constraint, so the session may be able to grow.
		r, c, ok := s.smallestLocked()
		return r, c, 0, 0, ok
	case ResizeFirstAttach:
		// Sizing moves to whoever is now earliest.
		next := s.earliestLocked()
		if next == nil {
			return 0, 0, 0, 0, false
		}
		cs := s.clientSizes[next]
		if cs.rows == 0 || cs.cols == 0 {
			return 0, 0, 0, 0, false
		}
		return cs.rows, cs.cols, cs.xpixel, cs.ypixel, true
	default:
		return 0, 0, 0, 0, false
	}
}

// SetResizePolicy sets which client owns the session's size.
func (s *Session) SetResizePolicy(p ResizePolicy) {
	s.mu.Lock()
	s.resizePolicy = p
	s.mu.Unlock()
}

// setRestored records a screen replayed from a previous incarnation's saved log.
func (s *Session) setRestored(blob []byte) {
	s.mu.Lock()
	s.restored = blob
	s.mu.Unlock()
}

// detach releases a subscriber, reporting whether it was the last one.
func (s *Session) detach(a attachment) (last bool) {
	a.reader.Close()
	if a.token != nil {
		if rows, cols, x, y, resize := s.releaseClientSize(a.token); resize {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err := s.Resize(ctx, uint32(rows), uint32(cols), uint32(x), uint32(y)); err != nil {
				s.log.Warn("resizing after a client detached failed", "error", err)
			}
		}
	}
	return s.clients.Add(-1) == 0
}

// ReportFocus tells the program in the session whether anyone is watching.
//
// Only sent when the program asked for focus events (DECSET 1004). Some programs use focus to
// decide whether to render at all, or whether to raise a desktop notification instead, and a
// detached session is exactly "nobody is watching". Without this a program keeps behaving as
// though it is on screen.
//
// Written as input rather than output because that is what a terminal does: focus reports travel
// the same direction as keystrokes.
func (s *Session) ReportFocus(ctx context.Context, focused bool) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil || !term.FocusReporting() {
		return
	}

	seq := "\x1b[O" // focus out
	if focused {
		seq = "\x1b[I"
	}
	_ = s.Write(ctx, []byte(seq))
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
	return s.resize(ctx, rows, cols, xpixel, ypixel, false)
}

// ResizeSignal is Resize, but guarantees the shell sees a window-size change even when the size is
// unchanged.
//
// Used on a fresh attach, where a program that repaints only on SIGWINCH would otherwise keep
// drawing against the snapshot just replayed. Not used for ordinary resizes, where the size really
// did change and the kernel signals on its own.
func (s *Session) ResizeSignal(ctx context.Context, rows, cols, xpixel, ypixel uint32) error {
	return s.resize(ctx, rows, cols, xpixel, ypixel, true)
}

func (s *Session) resize(ctx context.Context, rows, cols, xpixel, ypixel uint32, force bool) error {
	if _, err := s.shim.Resize(ctx, &shimv1.ResizeRequest{
		Rows: rows, Cols: cols, XPixel: xpixel, YPixel: ypixel,
		ForceSignal: force,
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

// Read renders the tail of the session's contents, with soft-wrapped lines optionally rejoined.
//
// Separate from History because the audiences differ: History exists so a person can page or pipe the
// whole scrollback, while this exists so a program can parse a bounded amount of it.
func (s *Session) Read(lines int, unwrap bool) ([]byte, error) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		// No emulator, so nothing can be rendered. Nil rather than an error, matching History: the
		// session works, this particular view does not.
		return nil, nil
	}
	return term.Tail(lines, unwrap)
}

// History renders the session's contents, scrollback included.
//
// Returns nothing rather than an error when there is no terminal model: a session without one
// works, it simply has no history to render, and failing would be a worse answer than empty.
func (s *Session) History(format HistoryFormat) ([]byte, error) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return nil, nil
	}
	switch format {
	case HistoryVT:
		return term.VT()
	case HistoryHTML:
		return term.HTML()
	default:
		return term.Plain()
	}
}

// HistoryFormat selects how History renders contents.
type HistoryFormat int

const (
	// HistoryPlain is plain text, for piping.
	HistoryPlain HistoryFormat = iota
	// HistoryVT preserves colors and styling as escape sequences.
	HistoryVT
	// HistoryHTML preserves styling as HTML.
	HistoryHTML
)

// Shutdown terminates the session's shell and its shim.
func (s *Session) Shutdown(ctx context.Context, force bool) error {
	_, err := s.shim.Shutdown(ctx, &shimv1.ShutdownRequest{Force: force})
	return err
}

// State queries the shim directly, which is the authority on whether a session is alive.
func (s *Session) State(ctx context.Context) (*shimv1.StateResponse, error) {
	return s.shim.State(ctx, &shimv1.StateRequest{})
}
