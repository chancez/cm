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
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/chancez/cm/internal/ansi"
	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/fault"
	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seq"
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
	// id is the session's identity, and the key it is registered under. Fixed for its whole life.
	id string
	// label is what to call this session when talking to a person: a name bound to it, or its ID with
	// the sigil so the value can be typed straight back. Display only, and deliberately not identity:
	// a name can be pointed at another session at any time, so anything that looks a session up has to
	// use id or it will find the wrong one.
	label  string
	record store.Session

	// conn and shim reach the session's shim. The server is the shim's only client.
	//
	// The interface rather than the concrete client, since the only thing done with it is closing: that
	// is what keeps the transport swappable without every holder naming it.
	conn transport.Conn
	shim shimv1.ShimClient

	// term accumulates terminal state so a reattaching client can be restored. Nil until
	// the VT layer lands; the fanout below does not depend on it.
	//
	// The pointer is guarded by mu, since the pump clears it when the model fails. Feeding it and
	// reading a snapshot from it are serialized by termMu instead.
	term Terminal

	// termMu pairs the terminal model's contents with modelSeq below.
	//
	// Separate from mu because the pump no longer feeds the model before waking clients: output is
	// delivered first and the model catches up afterwards, so the model can lag the log. Anything
	// that serializes a screen must therefore learn how far that screen goes, and must not be able
	// to see one without the other.
	//
	// Lock order is mu then termMu, never the reverse, and nothing may acquire mu while holding
	// termMu. That is why feedTerminal releases mu before taking termMu, and why the calls that
	// publish metadata happen after termMu is dropped.
	termMu sync.Mutex
	// modelSeq is the log position just past the last bytes the terminal model has consumed.
	//
	// A position in the log's numbering, not the shim's, because it is what a fresh attach streams
	// from after replaying a screen. Streaming from the log's end instead would silently drop
	// everything the model had not caught up to yet, which is output the client would never see.
	modelSeq seq.Log
	// outPartial holds the tail of a chunk that ends inside an unfinished escape sequence, and
	// outPartialSeq is that tail's position in the shim's numbering.
	//
	// The pump holds those bytes rather than passing them on, so every scanner downstream is looking at
	// whole sequences. Two of them were not, and each failed differently:
	//
	//   - RewritePromptRedraw skipped a prompt marker it received in pieces, so the client got redraw=1 and
	//     its terminal cleared the prompt on the next resize, waiting for a repaint that arrives in the
	//     pty's coordinates rather than the window's. The prompt is cleared and does not come back.
	//   - noteQueries did not record a terminal-only query it received in pieces. The stream is forwarded
	//     verbatim, so the client's terminal answered anyway and answerFromClient discarded the reply,
	//     nothing being outstanding to match it. The program that asked waits forever. Measured across
	//     seven query shapes, OSC 11 among them, which is the `wallfacer -h` hang the proxy exists for.
	//
	// Held before the graphics transform on purpose, so the bytes are still the shim's and lastSeq can
	// simply not count them. Holding after it would mean mapping post-transform lengths back to shim
	// positions, which is the two-numbering-spaces mistake in a new place.
	//
	// Owned by the pump goroutine alone, so no lock: every read and write is in processChunk.
	outPartial    []byte
	outPartialSeq seq.Shim

	// modelTrack follows the same bytes the model consumes, so modelPending below knows how much of an
	// incomplete sequence is sitting at the end of them. Guarded by termMu, like modelSeq.
	modelTrack ansi.Tracker
	// modelPending is how many bytes at the end of what the model consumed belong to a sequence that is
	// not finished.
	//
	// It exists because those bytes reach a client from nowhere. A model fed a partial sequence holds it in
	// its parser rather than on its screen, and Restore serializes the screen, so they are in neither the
	// snapshot a fresh attach replays nor the stream that follows it. Measured: a program writing
	// `ESC ] 2;fidelity BEL ESC [ 38:2:1` and then pausing left an attaching client with the title set and
	// `:2:3m` arriving as text, the nine bytes that opened the SGR gone from both halves. Failed about one
	// attach in eight.
	//
	// A count rather than a second position, deliberately. The pair "position, and the position of the last
	// boundary" has to be kept consistent by everything that sets either, and a Session built in a test
	// that set only modelSeq streamed from zero and replayed the whole log. A backlog whose zero value
	// means "nothing pending" is correct by default, which is the property worth having for a field that
	// exists to prevent a subtle bug.
	modelPending int

	// recent holds output consumed from the shim, so clients can subscribe from a
	// position rather than only receiving what arrives after they connect. Using the same
	// log type as the shim means the gap semantics are identical on both hops.
	recent *seqlog.Log[seq.Log]

	mu sync.Mutex
	// lastSeq is one past the last sequence number consumed from the shim, and is the
	// resume point if this server restarts.
	lastSeq seq.Shim
	// ended is set once the shim reports the shell exited.
	ended    bool
	exitCode int
	// title and cwd are what the shell last reported about itself, for clients and for
	// listing. cwdURI is the same directory as the shell sent it, keeping the host.
	title  string
	cwd    osc.Cwd
	cwdURI string
	// rawPwd is the last directory seen in the terminal model, which is what a change is measured
	// against. Kept so a repeat report can be recognized without re-parsing.
	//
	// Equal to cwdURI except while this session hosts a nested attach, when the model holds the
	// child's directory and cwd holds this session's own.
	rawPwd string
	// baseTitle is the last title seen in the terminal model, which is what a change is measured
	// against.
	//
	// Equal to title except while this session hosts a nested attach, when the model holds the
	// child's title and title deliberately holds this session's own. Separating them is what lets
	// the nesting end without the child's last title being mistaken for a change this session just
	// made. cwd needs no equivalent because rawPwd already plays this part for it.
	baseTitle string
	// command is what the shell last reported about itself via OSC 133: whether a command is running
	// and, when the shell says so, which one.
	//
	// Derived from the output stream rather than asked of the terminal model, because these are events
	// rather than state libghostty retains. A terminal has no "is a command running" to query.
	command osc.CommandState
	// baseCommand is the tracker's latest state, which a change is measured against.
	//
	// The counterpart of baseTitle, and separate from command for the same reason: while this session
	// hosts a nested attach the tracker follows the child's markers, and command must stay this
	// session's own. Comparing against the tracker alone would treat the child's last command as a
	// change to publish once the nesting ended.
	baseCommand osc.CommandState

	// reported is what a program inside the session said about itself, and takes precedence over
	// command above.
	//
	// Precedence, not merging: a program that says it is blocked knows something the shell cannot
	// express, since a shell reports a command as running whether it is computing or waiting at a prompt
	// of its own. Empty when nothing has reported, in which case the derived state stands.
	reported Reported
	// reportRuns counts how many times a report has changed the session's state.
	//
	// The counterpart of command.Runs, and needed for the same reason: a wait issued after sending input
	// has to tell "the state I am seeing describes my own work" from "it was already in that state". For
	// OSC 133 that question is answered by counting commands, and a session driven by reports alone never
	// increments it, so a `send --wait` against one could never observe a start and burned its whole
	// timeout on work that had already finished.
	reportRuns uint64

	// commands parses the OSC 133 markers out of the output stream.
	//
	// Outside mu deliberately: it is fed only by the pump, which is the single writer to terminal
	// state, so it needs no lock, and taking one per chunk of output would be on the hot path. The
	// state it produces is copied into command above, which is guarded.
	commands osc.CommandTracker

	// reports parses cm's own OSC sequence out of the output stream, which is how a shell integration
	// reports what OSC 133 cannot express.
	//
	// Outside mu for the same reason as commands, and fed from the same place. What it produces goes
	// into reported above, so a sequence and a `cm report` call end up in exactly the same field: the
	// sequence is a faster transport for the same statement, not a second kind of state.
	reports osc.ReportTracker

	// boundaries records where each command's output begins, so a caller can read back the last N
	// commands instead of guessing with a line count.
	//
	// Guarded by boundariesMu rather than left unlocked like the two trackers above, and the difference
	// is not an inconsistency: those are written by the pump and read only through fields it copies
	// under mu, while this is read directly by whichever goroutine is serving a Read RPC. One writer
	// and many readers still needs a lock.
	//
	// Its own mutex rather than mu, because a read of the boundaries must not contend with the
	// metadata that every chunk of output touches.
	boundariesMu sync.Mutex
	boundaries   *osc.BoundaryTracker

	// gfxScan pulls kitty graphics commands out of the output stream.
	//
	// Outside mu for the same reason as commands and reports: only the pump feeds it. Unlike those two
	// it *removes* bytes from what clients see, which the two sequence-number spaces already allow,
	// since the prompt rewrite beside it lengthens the stream. What must not happen is the shim's
	// numbering being computed from the shortened bytes, and it is not: lastSeq comes from the pump's
	// `data`, which is out.Data minus only a held-back partial marker, both of which are shim bytes.
	gfxScan graphics.Scanner
	// gfxStore keeps the payloads those commands carried, so images can be re-sent on attach.
	//
	// Its own lock inside, because an attaching client reads it while the pump writes. Separate from
	// libghostty's image storage on purpose: that holds decoded pixels for rendering, this holds the
	// compressed bytes a program sent, and rebuilding a transmission from decoded pixels was measured
	// at 90x the inbound size.
	gfxStore *graphics.Store

	// log records what this session does. Never nil.
	log *slog.Logger

	// resizePolicy decides which client sets the size. Empty behaves as ResizeLeader.
	resizePolicy ResizePolicy
	// clients tracks each attached client's size, keyed by its attachment.
	clientSizes map[*attachToken]*clientSize
	// evicts holds each attachment's eviction channel, so `cm detach` can reach a client that is
	// blocked waiting for output. Keyed by the same token as clientSizes, and removed by detach.
	evicts map[*attachToken]chan struct{}
	// upgrading marks the evictions that are upgrade requests rather than plain detaches, so the
	// streaming loop knows which kind of Detached to send.
	//
	// A side table rather than a field on the channel, because the channel is a bare signal and closing
	// it is the only thing that wakes a blocked client. Written before the close for that reason: the
	// reader wakes as soon as the channel closes, so a flag set afterwards would sometimes arrive too
	// late and the client would exit instead of coming back.
	upgrading map[*attachToken]bool
	// switching maps an eviction to the session reference the client should come back attached to, for
	// the evictions that are switches rather than upgrades.
	//
	// A reference rather than an ID because the caller decides which is right: an ordinary switch sends an
	// ID so the client lands on exactly that session, while a switch that rebound this window's name sends
	// the name, which can recreate the session if it has gone in the meantime where an ID would only
	// fail.
	switching map[*attachToken]string
	// queries holds each attachment's channel for questions cm needs that client to answer: the
	// background colour, the clipboard, the window's pixel size. Keyed like evicts, and removed by detach.
	//
	// A channel per client rather than the output log, because these bytes are addressed to one client
	// rather than to everything watching the session. Putting a question in the log would send it to every
	// attached terminal and each would answer, which is the duplicate-reply bug in a new costume, and it
	// would also record in the session's scrollback a question the shell never asked.
	//
	// Buffered, so the output pump is never blocked behind a slow client's socket. A full buffer drops the
	// question, which costs one unanswered query rather than a stalled session.
	queries map[*attachToken]chan []byte
	// requests is the ordered queue of outstanding proxied questions and replies waiting behind them.
	//
	// One queue per session rather than per client, because the ordering that must be preserved is the
	// *program's*: it asked its questions in an order down one pty and expects the answers in that order,
	// regardless of which client each question went to. See pendingRequest.
	requests []*pendingRequest
	// clock reads the current time, so a test can drive request expiry without sleeping. Nil means
	// time.Now.
	clock func() time.Time
	// leader is the attachment that last sent real typing, under ResizeLeader.
	leader *attachToken
	// attachOrder increases with each attach.
	attachOrder uint64
	// watched records that a client attached in order to display the session, as opposed to one that
	// attached only to create it or to stream its bytes. See EverWatched.
	watched bool

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

	// hosting counts the nested attachments running inside this session's shell, keyed by the
	// session each is attached to.
	//
	// While this is non-empty, nothing arriving in the output stream is a statement about *this*
	// session, and noteMetadata, noteCommand, and noteReport all decline to attribute it. The
	// reasoning is not about the bytes, which are indistinguishable, but about who can be speaking:
	// this session's shell is blocked inside `cm attach` for the whole time, so it reports nothing,
	// and every report on this pty therefore belongs to the child.
	//
	// A count per child rather than a bool, because attaching twice to the same session from inside
	// this one is legal, and the first of them detaching must not unfreeze a session the second is
	// still driving.
	hosting map[string]int

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
	// version and pid are what the client said about itself, for reporting only.
	//
	// Kept on this entry rather than in a parallel map because it is already the per-attachment record
	// and a second map keyed by the same token would be one more thing for detach to forget to clean.
	// Never used for a decision: both are advisory, an older client sends neither, and reporting an
	// empty value as unknown is better than inferring one.
	version string
	pid     int32
	// attachedAt is when this attachment became live, for reporting. Zero for a reservation.
	attachedAt time.Time
	// openedOnAlt records that this client attached while the session was on the alternate screen, so its
	// restore blob described that screen and nothing described the main one.
	//
	// It matters because the blob cannot carry the session's main screen: libghostty serializes the active
	// screen and GhosttyTerminalScreen is a read of which one that is, not a selector. So a client that
	// attaches mid-program has a main screen holding whatever its own window held before, and the ?1049l
	// the program sends on the way out pops the terminal onto it. The symptom is quitting vim and seeing
	// content from before the attach.
	openedOnAlt bool
	// repaint is signalled when the session leaves the alternate screen and this client is one that
	// attached during it. The attach loop selects on it and sends an empty output chunk flagged as a gap,
	// which makes the client drop its resume position and reattach; a fresh attach answers with a
	// serialized screen, which is the recovery wanted.
	//
	// A channel rather than a flag, for the reason evict is one, and the first version of this was the flag:
	// it was consumed by the next output chunk, and a program leaving the alternate screen usually produces
	// no more output, so the repaint waited for a byte that never came. Measured as a test failing about one
	// run in four, which is exactly how often the shell's next write happened to arrive separately.
	repaint chan struct{}
	// lastInputAt is when this client last sent something a person typed. Zero until it does.
	//
	// This is how cm identifies the client someone is actually using, and it is the only signal that
	// works. A client cannot be identified from inside the session: a command's stdout is the shim's
	// pty, which fans out to every attached client, so any escape sequence asking "which client are
	// you" is broadcast to all of them and answered by whichever replies first. That is the duplicate
	// reply bug the query proxy exists to prevent, in a new costume. Focus is no better: cm only hears
	// about focus if the program inside the session enabled DECSET 1004, so a session at a shell prompt
	// reports nothing, and zero focused clients is the ordinary case anyway once the window is behind a
	// browser.
	//
	// Typing has neither problem, because it is causal rather than inferred. A `cm clients upgrade`
	// typed at a prompt reached the shell as keystrokes on one specific attach stream, strictly before
	// the RPC arrived, so the client that sent them is the client that ran the command.
	//
	// Recorded whatever the resize policy is, which is why this is not simply Session.leader. Leadership
	// is only maintained under ResizeLeader, since claimLeadership returns early for the other three
	// policies, and "which window am I in" is a question worth answering under all four. Under the
	// default policy the two agree.
	//
	// Reporting only, and never used for a decision. Sizing still goes through the policy.
	lastInputAt time.Time
	// attached distinguishes a live attachment from a reservation that has not become one yet.
	//
	// Sizing deliberately does not care: reserveClient exists so the session can be resized to a
	// client's size *before* its screen is serialized, which means a reservation must be able to hold
	// a size and win the policy while it is still only a reservation.
	//
	// Answering a terminal query is the opposite. An entry that is not attached yet has no stream to
	// receive the query and no input channel to reply on, so counting it as an answerer means nobody
	// answers at all: cm stays silent because a client appears to be present, and the client never
	// sees the question because it was not subscribed when the query went past. See answererLocked.
	attached bool
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
	// SizeReport returns the in-band size report owed to the program for a resize to this size, or
	// nil when the program has not asked to be told about resizes in band (mode 2048).
	//
	// The size is passed in rather than read back, so this can be called before or after Resize and
	// still describe the new size.
	SizeReport(rows, cols uint16) ([]byte, error)
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
	// OnAltScreen reports whether the model is on the alternate screen, so the server can notice a
	// full-screen program leaving it. A client that attached during one holds no main-screen content.
	OnAltScreen() (bool, error)
	// KittyKeyboardProtocol reports whether a program has the kitty keyboard protocol enabled, so an
	// event in that encoding is one something asked for rather than one left over from a program that
	// has exited.
	KittyKeyboardProtocol() bool
	// Plain, VT, and HTML render the terminal contents, scrollback included, for a history
	// dump.
	Plain() ([]byte, error)
	// Tail renders the last lines as plain text, optionally rejoining soft-wrapped lines. A lines
	// value of zero means everything.
	Tail(lines int, unwrap bool) ([]byte, error)
	TailVT(lines int) ([]byte, error)
	VT() ([]byte, error)
	HTML() ([]byte, error)
	// Close releases emulator resources.
	Close() error
}

// maxHeldTail bounds the trailing partial sequence the pump will hold.
//
// A bound is needed for two different reasons, and the second is why it is small.
//
// A program can emit an introducer and then stop, and holding those bytes forever would withhold session
// output on a stream that is never going to complete the sequence. That argues only for some bound.
//
// The size argues for a small one: a kitty graphics transmission is an APC carrying a payload chunked at
// about 4 KiB, so a partial one is routinely larger than any query or prompt marker. Holding those would
// delay every image and buffer megabytes, and it would buy nothing, because the graphics scanner already
// reassembles a transmission across chunks. 256 bytes covers every sequence the scanners here care about,
// the longest being an XTGETTCAP request, and leaves graphics to the code that already handles it.
//
// Past the bound the tail is passed through, which is the behavior that existed before any holdback: no
// worse than it was, and it keeps a pathological stream from stalling a session.
const maxHeldTail = 256

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
// fromSeq is in the shim's numbering and clientSeq is in the numbering clients see. They are separate
// parameters because the prompt rewrite changes length, so the position to resubscribe from and the
// position to number the client log from are different numbers describing the same instant. Conflating
// them is what let a resuming client ask its new server for a position past the end of that server's
// log, where seqlog clamps forward and the bytes in between are lost.
func newSession(rec store.Session, term Terminal, fromSeq seq.Shim, clientSeq seq.Log) (*Session, error) {
	conn, shim, err := dialShim(rec.ShimSocket)
	if err != nil {
		return nil, err
	}

	pumpCtx, stopPump := context.WithCancel(context.Background())

	s := &Session{
		id: rec.ID,
		// Until a caller knows a name for it, which only the manager does.
		label:    paths.FormatSessionID(rec.ID),
		record:   rec,
		conn:     conn,
		shim:     shim,
		term:     term,
		recent:   seqlog.NewAt[seq.Log](DefaultRecentBytes, clientSeq),
		metaSubs: make(map[*metaSub]struct{}),
		// Positioned at the same offset as the log, since a session adopted after a server restart
		// resumes partway in and a tracker starting from zero would place every boundary wrongly.
		//
		// clientSeq rather than fromSeq: boundaries are recorded from the rewritten bytes, which is what
		// the log numbers, so a boundary stored in the shim's numbering would be off by the rewrite.
		boundaries: newBoundaryTrackerAt(clientSeq),
		// Not positioned like the boundary tracker above, because an image is addressed by the id the
		// program chose rather than by a position in the stream. A session adopted after a restart
		// therefore starts with no images and regains them as the program transmits, which is the same
		// bound the model has on its own storage.
		gfxStore:    graphics.NewStore(0),
		log:         cmlog.Discard(),
		clientSizes: make(map[*attachToken]*clientSize),
		evicts:      make(map[*attachToken]chan struct{}),
		queries:     make(map[*attachToken]chan []byte),
		hosting:     make(map[string]int),
		lastSeq:     fromSeq,
		// The model has consumed nothing, and where "nothing" is depends on where the log starts: an
		// adopted session resumes partway in. Leaving this at zero would make the first fresh attach
		// stream from the very beginning of the numbering, replaying output the restored screen already
		// shows.
		//
		// clientSeq, because this is a position in the client log: attach streams from it after replaying
		// a screen, and the log numbers rewritten bytes.
		modelSeq: clientSeq,
		done:     make(chan struct{}),
		stopPump: stopPump,
	}

	sub, err := shim.Subscribe(pumpCtx, &shimv1.SubscribeRequest{FromSeq: uint64(fromSeq)})
	if err != nil {
		stopPump()
		conn.Close()
		return nil, fmt.Errorf("subscribing to shim for %s: %w", rec.ID, err)
	}

	go s.pump(sub)
	// Expires proxied queries a client never answered, which is what keeps one unanswerable question from
	// holding every reply behind it. Ends with the session, via s.done.
	go s.runRequestSweeper()
	return s, nil
}

// pump consumes the shim's output stream, fans out to clients, and feeds the terminal model.
//
// This is the only writer to terminal state, so the emulator needs no locking of its own.
//
// Clients are woken before the emulator is fed, and that order is the point rather than an
// incidental detail. The terminal model is a derived cache: it exists for screen restore and for
// `cm read`, and no live client's output depends on it being current. Feeding it first put its cost
// in front of every keystroke's response, which was measurable and was reported as cm feeling slow
// in one direction only. `less` paging up emits a home plus reverse index per line, and a reverse
// index with the cursor on the top row was costing 14ms in the emulator against 10us for the same
// sequence anywhere else, so a half page spent about 350ms before the first byte reached the
// terminal. Paging down emits plain lines and was unaffected, which is why only "up is slow" was
// visible. The emulator cost itself is fixed separately, in how libghostty is built; this ordering
// is what keeps a slow model from being a slow session.
func (s *Session) pump(sub shimv1.Shim_SubscribeClient) {
	defer s.finish()

	for {
		out, err := sub.Recv()
		if err != nil {
			// The stream ends when the shell exits or the shim goes away. Either way this
			// session is over; which one is recorded by finish via a State call.
			return
		}

		s.processChunk(out.Data, seq.Shim(out.Seq))
	}
}

// processChunk is one chunk of shim output, from arrival to the model.
//
// Extracted from pump so the ordering in it can be tested directly. The pump reads from a ttrpc stream, so
// the alternative was driving a real shim for every question about what a chunk does, which cannot express
// "this chunk ends inside a sequence" without racing a pty.
//
// Called only from pump, on pump's goroutine, which is what lets promptPartial and the trackers go without
// locks.
func (s *Session) processChunk(raw []byte, rawSeq seq.Shim) {

	// A chunk ending inside an escape sequence is trimmed, and the tail waits for the rest.
	//
	// Everything below reads whole sequences or none, which several scanners assumed and only the OSC 133
	// trackers arranged for themselves. Done here, before the graphics transform, so the held bytes are
	// still the shim's and lastSeq can decline to count them; a restarting server resubscribes from before
	// them and the shim sends them again, which matches a log that never received them.
	//
	// data replaces raw for the rest of this function. Using raw anywhere below would feed a consumer
	// bytes the log is not going to receive yet, which is how the two numbering spaces drift.
	data := raw
	base := rawSeq
	if len(s.outPartial) > 0 {
		// Prepended, so the sequence is whole again and its position is where the partial began.
		data = append(s.outPartial, raw...)
		base = s.outPartialSeq
		s.outPartial = nil
	}
	if held := ansi.PartialTailLen(data); held > 0 && held <= maxHeldTail {
		s.outPartial = append([]byte(nil), data[len(data)-held:]...)
		s.outPartialSeq = base + seq.Shim(len(data)-held)
		data = data[:len(data)-held]
	}
	if len(data) == 0 {
		// The whole chunk was the start of a sequence. Nothing has been consumed, so nothing advances:
		// not the trackers, not the log, not lastSeq.
		return
	}

	// Track what the shell says about itself from the bytes as the shell sent them, before the
	// rewrite below, so the markers are read exactly as written.
	//
	// This is where "is a command running" comes from. The shell reports it via OSC 133 as part of
	// its normal output, so cm reads it in passing rather than asking the shell to maintain it: zmx
	// needed shell hooks writing a label for the same information.
	//
	// Kept ahead of the append so a waiter sees a command start no later than the output it
	// produced. A client that learned of output first and of the command afterwards could observe
	// a command's own bytes while the session still claimed to be idle.
	// Fed regardless of nesting, so the trackers stay in sync with the byte stream: a chunk
	// skipped here can split a sequence across the resume and leave the next real report
	// unrecognizable. What nesting suppresses is the attribution, inside noteCommand and
	// noteReport, not the parsing.
	if s.commands.Feed(data) {
		s.noteCommand()
	}

	// cm's own sequence, read from the same stream for the same reason: a shell integration writes it
	// straight to the pty, which costs nothing, where shelling out to `cm report` from a prompt hook
	// costs about 23ms twice per command. That was measured before choosing this design, and it is the
	// whole reason the sequence exists rather than only the command.
	if s.reports.Feed(data) {
		s.noteReport()
	}

	// Terminal queries are deliberately *not* removed from the stream, and two earlier versions of
	// this code removed them, so the reasons are worth recording.
	//
	// The first stripped them unconditionally to make cm the only answerer. That fixed a duplicate
	// DA1 reply but left the real defect, cm injecting answers mid-read of an unrelated program, and
	// it silenced queries an attached terminal was going to answer. The second stripped only what cm
	// answered, which was correct about who answers and wrong about byte counts: removing bytes makes
	// the client log shorter than the shim's numbering accounts for, which inverted the resume
	// ordering and clamped a reconnecting client into the middle of an escape sequence.
	//
	// So the stream is forwarded verbatim, and the fix is in *who writes replies* instead. cm is now
	// the only writer of a reply to this pty: it answers what its model can answer, and for a query
	// only a real terminal can answer it asks one client and relays what comes back. A client never
	// answers directly, so a query reaching several clients, or reaching one twice across a restart,
	// cannot produce a second answer.
	//
	// Kitty graphics is taken out of the stream and re-emitted by cm, which is the one protocol cm
	// consumes rather than forwards. The reason is that a transmission may name a *file* and the
	// file is consumed once, so forwarding one lets the program and the real terminal race for it:
	// the reported "EBADF ... No such file or directory" on exactly the probes that name a path.
	// See docs/architecture.md on what cm presents itself as.
	//
	// Ahead of the prompt rewrite because a graphics payload is arbitrary base64 that may contain
	// the bytes the rewrite matches on, and rewriting inside a payload would corrupt the image.
	//
	// Ahead of noteQueries too, and that ordering is load-bearing rather than incidental. cm rewrites
	// a graphics query when it resolves a transfer, so the command the *terminal* is asked is not the
	// one the program wrote. noteQueries has to register the rewritten form, or the reply arriving
	// from the terminal is matched against a question nobody asked and discarded as unsolicited. This
	// used to run after noteQueries, on the reasoning that a graphics command was not a proxied query
	// at all, which stopped being true when a=q joined the terminal-only set.
	gfxData := data
	if segs := s.gfxScan.Scan(data); segs != nil {
		gfxData = s.handleGraphics(segs)
	}

	// Registered before the model is fed, which is the load-bearing ordering here. The model
	// generates replies to answerable queries synchronously inside its Write, so a question later in
	// this same chunk must already be outstanding by then or the reply would be written straight to
	// the pty and overtake it. See noteQueries and queueOrWriteReply.
	//
	// Fed the post-graphics bytes for the reason above: these are what reach the terminal, so these
	// are the questions it will answer.
	s.noteQueries(gfxData)

	// Forcing redraw=0 into prompt markers before anything else sees them. A multiplexer
	// sits between the shell and the outer terminal, so a terminal that trusts the shell to
	// repaint its prompt clears it and never gets a usable repaint back.
	rewritten := osc.RewritePromptRedraw(gfxData)

	// Command boundaries come from the rewritten bytes, not the originals.
	//
	// This is the load-bearing ordering in this function, and getting it wrong is silent. The log
	// below numbers exactly these bytes, and RewritePromptRedraw can make a prompt marker nine bytes
	// longer than the shell sent, so feeding the pre-rewrite chunk instead would drift every
	// recorded position by nine bytes per prompt and a read would start mid-sequence. Fed here
	// rather than beside s.commands.Feed above for that reason alone.
	s.boundariesMu.Lock()
	s.boundaries.Feed(rewritten)
	s.boundariesMu.Unlock()

	// Appending both records the output for later subscribers and wakes current ones.
	// A slow client cannot stall the session: it simply falls behind and is told there
	// is a gap if the window passes it.
	s.recent.Append(rewritten)

	// The window resumePoints documents: the chunk is in the log and lastSeq does not account for it
	// yet. No lock closes it, so a test widens it instead.
	fault.At(fault.AfterLogAppend)

	// Two sequence numbers, deliberately, because the transforms above change the length: the
	// prompt rewrite lengthens, and the query strip shortens.
	//
	// lastSeq tracks the shim's numbering, since it is the position to resubscribe from after
	// a restart, and the shim knows nothing about either transform. It is therefore computed from
	// `data`, never from the stripped or rewritten bytes. Clients are served from s.recent,
	// which numbers what they actually receive.
	//
	// `data` rather than out.Data, and the difference is a partial prompt marker held back at the top
	// of the loop. Those bytes are still the shim's, so declining to count them is exactly right: a
	// restart resubscribes from before them and the shim sends them again, matching a log that never
	// received them. Counting them would skip them on the next server, which is the same hole this
	// pair of numbers exists to prevent.
	//
	// Conflating them desynchronizes the two by however much the rewrite added, which puts a
	// client's resume position inside an escape sequence and slices the ESC off the front of
	// it. The visible result is a cursor move rendering as literal text beside the prompt.
	s.mu.Lock()
	// Counted from what was consumed rather than from what arrived, so bytes held back for an
	// unfinished marker are re-sent after a restart instead of being skipped.
	s.lastSeq = base + seq.Shim(len(data))
	s.mu.Unlock()

	// The model is fed gfxData rather than out.Data, which is a change from feeding the shell's
	// original bytes and is required rather than incidental: a graphics command cm removed from the
	// client stream must also be removed from what the model sees, or the model would store an image
	// from a command naming a file it cannot read, and its idea of the screen would disagree with
	// every client's. The inlined replacement cm produced is in gfxData, so both see the same thing.
	//
	// The prompt rewrite is still not applied here, which is the existing asymmetry this preserves:
	// the model gets the markers as the shell wrote them, and only clients see redraw=0.
	// The model-lag window: clients have this chunk and the model does not. Both bugs that lived here
	// were found by chance, one at about one attach in eight.
	fault.At(fault.BeforeModelFeed)

	s.feedTerminal(gfxData, s.recent.Next())
}

// graphicsRestore builds the commands that re-send this session's images to an attaching client.
//
// Empty when nothing has been transmitted, which is the common case and costs one map read.
//
// Sent to every attaching client rather than tracked per client. zellij keeps per-client state so it can
// skip re-sending to a client that already has an image, which is worth it there because it re-emits
// every frame; cm sends only on attach, so the bookkeeping would cost more than the bytes it saves. The
// consequence is that a second client attaching to a session receives the images again, which is correct
// rather than merely acceptable: it has never seen them.
func (s *Session) graphicsRestore() []byte {
	// Nil for a Session built field by field rather than through newSession, which several tests do to
	// hold an exact intermediate state. Treated as "no images" rather than fixed up, for the same reason
	// the evicts map is created lazily on attach: a panic here would surface a long way from the missing
	// line that caused it.
	if s.gfxStore == nil {
		return nil
	}
	rt := s.gfxStore.Retransmissions()
	if len(rt) == 0 {
		return nil
	}
	var out []byte
	for _, r := range rt {
		out = append(out, r.Bytes...)
	}
	s.log.Debug("re-sending graphics images on attach",
		"session", s.label, "images", len(rt), "bytes", len(out))
	return out
}

// handleGraphics consumes the graphics commands found in a chunk and returns the bytes to forward.
//
// Each command is resolved and re-emitted rather than passed through. A transmission naming a file is
// read here and rebuilt carrying its data, so the terminal receives something it can satisfy without
// racing cm for a single-use file. A payload is kept so the image can be re-sent on a later attach.
//
// A command cm cannot resolve is dropped rather than forwarded, and that is deliberate: forwarding it
// would put the program and the terminal back in the race this exists to remove, and the program has a
// fallback for a declined medium. `kitten icat` negotiates stream and exits 0 when a medium is refused,
// measured against a control of the same kitty with no cm.
// Segments are walked in order rather than emitting the ordinary bytes and then the commands, and the
// difference is a corruption rather than a nicety: a chunk of "text cmd text" rebuilt the wrong way puts
// the command after all of the text. Observed in a sandbox as a refused command's payload printed on the
// prompt line, with the probe beside it arriving payload-free and kitty answering
// "ENODATA: Insufficient image data: 0 < 3".
func (s *Session) handleGraphics(segs []graphics.Segment) []byte {
	out := make([]byte, 0, 256)

	// Same reason as graphicsRestore: a Session built field by field has no store, and dropping the
	// bookkeeping is better than panicking on the output path, where the failure would end the session.
	store := s.gfxStore

	for _, seg := range segs {
		if !seg.Graphics {
			out = append(out, seg.Data...)
			continue
		}

		resolved, err := graphics.ReadTransfer(seg.Cmd)
		if err != nil {
			// Dropped, and the whole command goes with it. Emitting any part of it is what produced the
			// leak above, and forwarding all of it would put the program and the terminal back in the
			// race for a single-use file.
			//
			// Logged rather than silent, because an image that does not appear is otherwise inexplicable,
			// and swallowed advisory failures are what cm's diagnostic logs are for.
			s.log.Info("declined a graphics transfer",
				"session", s.label, "medium", string(seg.Cmd.Medium), "error", err)
			continue
		}

		if store != nil {
			if resolved.IsTransmission() {
				store.Add(resolved)
			} else if id, byNumber, ok := resolved.Key(); ok {
				// A command that places or otherwise uses an image counts as touching it, so eviction
				// does not drop what is currently on screen.
				store.Touch(id, byNumber)
			}
		}

		out = append(out, resolved.Raw...)
	}
	return out
}

// feedTerminal advances the terminal model, recording how far into the log it has consumed.
//
// modelEnd is the log position just past the bytes being fed. The model is fed the shell's original
// bytes while the log holds the rewritten ones, so the two differ in length; what is recorded is the
// log position the model's screen now corresponds to, which is what attach needs to stream from.
//
// Its own method because the pair must be atomic with respect to attach. attach serializes a screen
// and picks a position to stream from, and if it could see the model updated but the position not
// yet, it would replay a chunk the client is also about to receive.
func (s *Session) feedTerminal(data []byte, modelEnd seq.Log) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return
	}

	// Read before the write so the transition out of the alternate screen can be seen. Cheap: a mode
	// query rather than a serialization.
	wasOnAlt, altErr := term.OnAltScreen()

	s.termMu.Lock()
	err := term.Write(data)
	if err == nil {
		s.modelSeq = modelEnd
		// Fed the same bytes as the model, so the boundary is expressed in the same positions. modelEnd
		// minus the tracker's own backlog is where the last complete sequence ended.
		s.modelTrack.Feed(data)
		s.modelPending = int(s.modelTrack.Fed() - s.modelTrack.Boundary())
	}
	s.termMu.Unlock()

	// A full-screen program has quit. Any client that attached while it was running holds no main-screen
	// content, so the ?1049l in this very chunk has just switched its terminal onto whatever its own window
	// held before the attach. Those clients are flagged for a repaint; the rest are left alone, since a
	// client attached before the program started has the right main screen and repainting it would be a
	// flicker for nothing.
	if err == nil && altErr == nil && wasOnAlt {
		if nowOnAlt, nErr := term.OnAltScreen(); nErr == nil && !nowOnAlt {
			s.markAltScreenLeft()
		}
	}

	if err != nil {
		// A terminal model that cannot keep up would make restores wrong, but dropping the
		// session over it would be worse: live output still works. Give up on restores instead
		// by discarding the model.
		s.log.Error("terminal model failed, screen restore disabled for this session",
			"session", s.label, "error", err)
		s.mu.Lock()
		s.term = nil
		s.mu.Unlock()
		return
	}

	// Both take s.mu, and drainPending calls the shim, so neither may run under termMu.
	s.drainPending()
	s.noteMetadata()
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

// drainPending sends replies the emulator generated to the pty, in the order the program asked.
//
// These answer questions a program asked the terminal, such as a device status report. Without them a
// program that queried the terminal waits for a reply that never comes, so they are always delivered:
// unconditionally now, where this used to be skipped whenever an attached client looked able to answer.
//
// That condition is gone because clients no longer answer. cm asks a client only for the queries its own
// model cannot answer, and relays the reply itself, so cm is the single writer of every reply on this pty.
// The condition existed to prevent two answerers, and one writer removes the possibility rather than
// managing it. What it cost was four separate bugs, each a case where the election was wrong: a read-only
// follower counted as an answerer and nothing replied; a reserved-but-unattached client counted and
// nothing replied; two attached clients both replied; and across a restart cm replied and the reconnecting
// client replied to the same query from the log.
//
// The ordering is the part that is not obvious and is the reason this defers to a queue. A program that
// asks two questions expects two answers in order, and cm can answer some immediately while others need a
// round trip to a client. Writing the fast one first reorders the conversation, and a program reading
// positionally then takes the wrong answer for its question. That is the recorded `wallfacer -h`
// corruption: wallfacer blocked reading an OSC 11 answer, a zsh prompt hook's CSI 6n was answered by the
// emulator in the meantime, and the cursor report cm injected was consumed by wallfacer as though it were
// the background colour. The real reply then arrived unclaimed and the line editor printed
// ";rgb:2828/2c2c/3434". queueOrWriteReply holds a local reply behind any outstanding question so this
// cannot happen.
//
// TakePending is called unconditionally and before anything else, so the emulator's queue is always
// emptied. Leaving bytes in it would deliver them at some later, unrelated moment.
func (s *Session) drainPending() {
	if s.term == nil {
		return
	}
	s.queueOrWriteReply(s.term.TakePending())
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
	// While a nested attach is running in this session, these values came from the child and say
	// nothing about this session. Rebaselined rather than merely ignored, and that is the whole
	// difference between a fix and a fix that looks like one: the model has genuinely absorbed the
	// child's OSC 7 and OSC 2, so leaving the previous baseline in place would make the child's
	// value look like a fresh change on the first call after the nesting ends, publishing it then
	// instead of now.
	if len(s.hosting) > 0 {
		s.rawPwd = rawPwd
		s.baseTitle = title
		s.mu.Unlock()
		return
	}
	titleChanged := title != s.baseTitle
	pwdChanged := rawPwd != s.rawPwd
	if titleChanged {
		s.baseTitle = title
		s.title = title
	}
	if pwdChanged {
		s.rawPwd = rawPwd
		s.cwdURI = rawPwd
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
	// A nested attach means this marker came from the child session, not this shell. Only the
	// baseline moves, so the published value stays this session's own and the child's last command
	// is not republished as a change the moment the nesting ends.
	//
	// Freezing command also freezes command.Runs, which is the point rather than a side effect.
	// Those counters exist so a `send --wait` can tell the caller's own work from the state a
	// session was already in, and a child's commands incrementing the parent's count is what let a
	// `cm wait` on the parent be satisfied by the child.
	if len(s.hosting) > 0 {
		s.baseCommand = state
		s.mu.Unlock()
		return
	}
	if state == s.baseCommand && state == s.command {
		s.mu.Unlock()
		return
	}
	s.baseCommand = state
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

// noteReport applies a report the shell integration wrote into the output stream.
//
// Routed through setReported so a sequence and a `cm report` call are indistinguishable once received.
// They are two transports for one statement, and keeping one destination means a waiter cannot see
// different behavior depending on which the reporter happened to use.
//
// "clear" becomes the zero value, which is how setReported already expresses "nothing has reported": the
// session falls back to what cm derives from OSC 133. Mapping it here rather than storing "clear" as a
// state keeps that vocabulary out of everything downstream, none of which should have to know that one
// state name means the absence of a state.
func (s *Session) noteReport() {
	r, ok := s.reports.Take()
	if !ok {
		return
	}
	// A report arriving while a nested attach is running was written by a program inside the child,
	// which reached this pty only because the child's output passes through it. Dropped rather than
	// baselined: unlike a title or a directory this is an event rather than a value, so there is no
	// baseline to keep, and the child's own server has already recorded it against the right session.
	//
	// This is the case that made the bug more than cosmetic. With `cm wait outer --until blocked`
	// running, a blocked report sent into the inner session satisfied the outer wait.
	s.mu.Lock()
	nested := len(s.hosting) > 0
	s.mu.Unlock()
	if nested {
		return
	}
	if r.State == "clear" {
		s.setReported(Reported{})
		return
	}
	s.setReported(Reported{State: r.State, Detail: r.Detail, Source: r.Source})
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
	// Counted on every change, so a wait can tell a state describing the caller's own work from the state
	// the session was already in. Incremented for a clear too: withdrawing a report is a change like any
	// other, and a caller waiting after one still needs to know something happened.
	s.reportRuns++
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

// SnapshotFrom returns the session's retained output from a sequence number.
//
// Raw bytes, so a caller can either render them or hand them over as they are. Rendering needs a
// terminal model built at the right size, which the manager owns rather than the session, since a
// session's own model holds the *current* screen and replaying into it would corrupt what clients see.
//
// gap reports that output at or after from had already been trimmed, so the result starts later than
// asked for.
func (s *Session) SnapshotFrom(from seq.Log) (data []byte, gap bool) {
	return s.recent.Snapshot(from)
}

// Size reports the dimensions to render a snapshot at.
//
// From the record rather than from a live client, because a snapshot may cover output produced when a
// different client was attached, or none was. The record is what the session was created or last
// resized at, which is the width the bytes were wrapped for. Falls back to a conventional size, matching
// what the replay-from-disk path does for the same reason.
func (s *Session) Size() (rows, cols uint16) {
	rows, cols = uint16(s.record.Rows), uint16(s.record.Cols)
	if rows == 0 || cols == 0 {
		return 24, 80
	}
	return rows, cols
}

// newBoundaryTrackerAt builds a boundary tracker positioned at a starting offset.
//
// A helper rather than two lines inline, so the pairing of construction and positioning is in one place:
// a tracker built without SetPosition silently records every boundary as an offset from zero, which for
// an adopted session is wrong by however far into its life the server restarted.
// fromSeq is in the log's numbering, which is the trap this function exists to make explicit:
// boundaries are recorded from the rewritten bytes, and taking them from the pre-rewrite stream drifts
// by nine bytes per prompt marker. internal/osc still counts in plain uint64, so the space stops here.
func newBoundaryTrackerAt(fromSeq seq.Log) *osc.BoundaryTracker {
	t := osc.NewBoundaryTracker(0)
	t.SetPosition(uint64(fromSeq))
	return t
}

// ErrNoCommandBoundaries reports that a session has never bracketed a command, so there is no position
// to read back from.
//
// Its own error because the cause is almost never "nothing ran". A shell with no OSC 133 integration
// loaded reports no markers at all, which is the same condition `cm doctor`'s no-shell-integration check
// exists for, and returning empty output would look like a command that printed nothing.
var ErrNoCommandBoundaries = errors.New("session has not reported any command boundaries")

// SinceCommands returns the position where the last n command blocks begin.
//
// Anchored at the prompt, so a read from here includes the prompt and the echoed command line. That is
// what makes reading several commands useful: their outputs concatenated with nothing between them
// cannot be told apart.
//
// available reports how many command blocks are known when n exceeds them, so a caller can say how many
// there are rather than quietly returning fewer than asked for.
func (s *Session) SinceCommands(n int) (pos seq.Log, available int, err error) {
	s.boundariesMu.Lock()
	defer s.boundariesMu.Unlock()

	// Named at rather than seq, which would shadow the package naming the two spaces. internal/osc
	// counts in plain uint64, so the space is applied here: a boundary is a position in the log's
	// numbering, because the tracker is fed the rewritten bytes.
	at, available, ok := s.boundaries.SinceCommands(n)
	if ok {
		return seq.Log(at), available, nil
	}
	if available == 0 {
		return 0, 0, ErrNoCommandBoundaries
	}
	return 0, available, fmt.Errorf(
		"only %d command(s) are known for session %s", available, s.label)
}

// LastOutput returns the position where the most recent command's own output begins.
//
// Excludes the prompt and the echoed command line, unlike SinceCommands, which is the difference between
// a transcript and something a parser can read directly.
func (s *Session) LastOutput() (seq.Log, error) {
	s.boundariesMu.Lock()
	defer s.boundariesMu.Unlock()

	at, ok := s.boundaries.LastOutput()
	if !ok {
		return 0, ErrNoCommandBoundaries
	}
	return seq.Log(at), nil
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

// StateRuns counts every observable change to what the session says it is doing, from either source.
//
// Both are summed rather than tracked separately because a caller asking "has anything happened since I
// sent this" does not care which mechanism reported it, and an agent may well use both: a shell emitting
// OSC 133 while the program inside it reports its own state. Either moving is a start.
func (s *Session) StateRuns() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.command.Runs + s.reportRuns
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

// beginHosting records that a client attached to child is running inside this session's shell.
//
// From that point until endHosting, everything in this session's output stream is the child's:
// its OSC 7, OSC 2, and OSC 133 all travel through this pty because the nested client's stdout is
// this session's terminal. The bytes are indistinguishable from this shell's own, so cm cannot
// filter them, but it does not need to. This shell is blocked inside `cm attach` for the whole
// interval and reports nothing, so attribution can be suspended wholesale.
//
// Returns whether this session is now hosting anything, so the caller can log a transition rather
// than every attach.
func (s *Session) beginHosting(child string) {
	s.mu.Lock()
	s.hosting[child]++
	s.mu.Unlock()
}

// endHosting records that a nested attachment to child has finished.
//
// The published title, directory, and command are left exactly as they were, which is the whole
// point: they are this session's last true values, since nothing it reported was ever overwritten.
// The baselines were kept current throughout, so the next thing this shell reports registers as a
// change while the child's final values do not.
func (s *Session) endHosting(child string) {
	s.mu.Lock()
	if n := s.hosting[child]; n > 1 {
		s.hosting[child] = n - 1
	} else {
		delete(s.hosting, child)
	}
	s.mu.Unlock()
}

// Hosting returns the sessions currently attached from inside this one, sorted.
//
// Sorted so a listing is stable between calls rather than following Go's map ordering, which would
// make `cm list` reorder the field on every invocation.
func (s *Session) Hosting() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.hosting) == 0 {
		return nil
	}
	out := make([]string, 0, len(s.hosting))
	for name := range s.hosting {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// CwdURI returns the directory exactly as the shell reported it, which for OSC 7 is a URI that
// keeps the host.
//
// Worth exposing alongside the decoded path: the host is what distinguishes a session that has
// ssh'd elsewhere, and a caller shown only a decoded path cannot tell.
func (s *Session) CwdURI() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cwdURI
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
func (s *Session) LastSeq() seq.Shim {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastSeq
}

// ClientSeq returns the position clients have been served up to, in the numbering they see.
//
// Distinct from LastSeq, which counts the shim's bytes. The prompt rewrite lengthens output, so the
// two diverge, and an adopting server needs this one to position its client log. See resumePoints for
// why the pair is usually read together rather than one at a time.
func (s *Session) ClientSeq() seq.Log {
	return s.recent.Next()
}

// resumePoints returns both resume positions, for storing as a pair.
//
// The two count the same output in different numbering, so they are read here rather than by two calls
// from each caller that stores them.
//
// The read order is deliberate and cannot be replaced by a lock. The pump appends to the client log
// *before* it takes s.mu to advance lastSeq, so a chunk can be in the log while lastSeq does not yet
// account for it, and no lock available here closes that window.
//
// Reading lastSeq first is what makes the window harmless. It can only make the stored pair describe
// a client log that is at or ahead of what the shim position accounts for, so the next server
// resubscribes from slightly before where its log starts and re-delivers the overlap. Reading in the
// other order would let the shim position be ahead instead, so those bytes would never be requested
// while the log numbering already counted them, and a client resuming there would be served from a
// position whose bytes never arrive. Duplicated output is visible and recoverable; a silent hole in
// the middle of an escape sequence is the corruption this pair exists to prevent.
func (s *Session) resumePoints() (shimSeq seq.Shim, clientSeq seq.Log) {
	s.mu.Lock()
	shimSeq = s.lastSeq
	s.mu.Unlock()
	// Deliberately after s.mu is released: the log has its own lock, and attach takes s.mu before
	// touching the log, so acquiring them the other way round here would invert that order.
	return shimSeq, s.recent.Next()
}

// markAltScreenLeft flags every client that attached during a full-screen program for a repaint.
//
// Called when the model leaves the alternate screen. Only those clients: one attached beforehand already
// holds the main screen the session is returning to, and repainting it would be a visible flicker with
// nothing behind it.
func (s *Session) markAltScreenLeft() {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, cs := range s.clientSizes {
		if !cs.openedOnAlt {
			continue
		}
		// Cleared first, so a program that takes the alternate screen again later does not repaint this
		// client a second time: by then it has a main screen from the repaint this one triggers.
		cs.openedOnAlt = false
		if cs.repaint == nil {
			continue
		}
		// Non-blocking: the loop may be between selects, and a repaint already pending is the same repaint.
		select {
		case cs.repaint <- struct{}{}:
		default:
		}
	}
}

// repaintChan returns the channel this client is signalled on when it needs repainting, or nil.
func (s *Session) repaintChan(tok *attachToken) <-chan struct{} {
	if tok == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	cs := s.clientSizes[tok]
	if cs == nil {
		return nil
	}
	return cs.repaint
}

// Clients reports how many clients are attached.
func (s *Session) Clients() int64 { return s.clients.Load() }

// AttachedClientInfo is one attached client, for reporting.
type AttachedClientInfo struct {
	PID        int32
	Version    string
	ReadOnly   bool
	AttachedAt time.Time
	// LastInputAt is when this client last sent typing, zero if it never has. See clientSize.lastInputAt
	// for why typing is the signal.
	LastInputAt time.Time
	// Active marks the client that typed most recently, which is the one someone is using. At most one
	// client per session has it, and none does when nothing has typed yet.
	//
	// Computed by the session rather than left to the caller, because deciding it needs every client's
	// timestamp at once and a caller holding only its own row cannot. A CLI comparing timestamps itself
	// would also have to re-derive the ties-go-to-nobody rule below.
	Active bool
}

// noteClientIdentity records what a client said about itself, for reporting only.
//
// Separate from attach rather than a parameter on it, because attach is called by paths that have no
// client to describe: a reservation, and the internal attachments the tests and `cm run` use. A no-op
// for an unknown token, which is what a detach racing the identity arriving looks like.
func (s *Session) noteClientIdentity(tok *attachToken, version string, pid int32) {
	if tok == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs := s.clientSizes[tok]; cs != nil {
		cs.version, cs.pid = version, pid
	}
}

// noteClientInput records that this client sent typing, so it can be reported as the active one.
//
// Separate from claimLeadership, which is called on the same keystrokes, because the two answer
// different questions and must not be merged. Leadership is a *decision* about the pty's size, gated on
// the configured policy and refused to a follower. This is a *record* of who is being used, wanted under
// every policy. Folding this into claimLeadership would silently stop recording for anyone running
// resize_policy other than the default, since that function returns early for the other three.
//
// A no-op for an unknown token, which is a detach racing a keystroke that was already in flight.
func (s *Session) noteClientInput(tok *attachToken) {
	if tok == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs := s.clientSizes[tok]; cs != nil {
		cs.lastInputAt = s.now()
	}
}

// AttachedClients describes the clients attached right now.
//
// Reservations are excluded: one is a client that has not attached yet and may never, and reporting it
// would say a session has a client watching it when nothing is. That distinction is the same one the
// query proxy had to learn, where counting a reservation as an attachment left a program's query
// unanswered. Sizing is the only thing that deliberately counts reservations.
//
// Ordered by attach order so repeated calls agree, since Go randomizes map iteration and a listing that
// reshuffles its own output on every call is hard to read and impossible to diff.
func (s *Session) AttachedClients() []AttachedClientInfo {
	s.mu.Lock()
	defer s.mu.Unlock()

	type entry struct {
		order uint64
		info  AttachedClientInfo
	}
	active := s.activeClientLocked()

	entries := make([]entry, 0, len(s.clientSizes))
	for tok, cs := range s.clientSizes {
		if !cs.attached {
			continue
		}
		entries = append(entries, entry{cs.order, AttachedClientInfo{
			PID:         cs.pid,
			Version:     cs.version,
			ReadOnly:    cs.readOnly,
			AttachedAt:  cs.attachedAt,
			LastInputAt: cs.lastInputAt,
			Active:      active != nil && tok == active,
		}})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].order < entries[j].order })

	out := make([]AttachedClientInfo, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.info)
	}
	return out
}

// activeClientLocked returns the attachment that typed most recently, or nil when none can be named.
//
// One definition, shared by the listing and by `cm clients upgrade --current`, so the mark a user sees
// and the client an upgrade acts on cannot disagree. Two copies of this comparison would be two chances
// to drift, and the drift would be invisible: the listing would star one window while the upgrade
// repainted another.
//
// Nil in three cases, and all three are deliberately not guesses. Nothing has typed yet, so no window
// has been used. Only reservations are present, which are clients that may never attach. Or two
// timestamps tie, which cannot happen from real keystrokes and means an injected clock that does not
// advance; picking one by map order would move the mark between identical calls.
//
// Followers are eligible only in the sense that they can never win: a read-only client's input is
// dropped before it reaches here, so its lastInputAt stays zero and it is skipped for that reason
// rather than by a readOnly test. Checking readOnly here as well would be a second rule to keep in step
// with the input path.
func (s *Session) activeClientLocked() *attachToken {
	var (
		best *attachToken
		when time.Time
		tied bool
	)
	for tok, cs := range s.clientSizes {
		if !cs.attached || cs.lastInputAt.IsZero() {
			continue
		}
		switch {
		case best == nil || cs.lastInputAt.After(when):
			best, when, tied = tok, cs.lastInputAt, false
		case cs.lastInputAt.Equal(when):
			tied = true
		}
	}
	if tied {
		return nil
	}
	return best
}

// ActiveClient describes the client someone is using, and whether one could be named.
//
// Exposed separately from AttachedClients because a caller asking about one client should not have to
// scan a list and re-apply the rule. The bool distinguishes "no active client" from a zero struct, which
// a client that has typed but reported no pid or version would otherwise be indistinguishable from.
func (s *Session) ActiveClient() (AttachedClientInfo, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	tok := s.activeClientLocked()
	if tok == nil {
		return AttachedClientInfo{}, false
	}
	cs := s.clientSizes[tok]
	return AttachedClientInfo{
		PID:         cs.pid,
		Version:     cs.version,
		ReadOnly:    cs.readOnly,
		AttachedAt:  cs.attachedAt,
		LastInputAt: cs.lastInputAt,
		Active:      true,
	}, true
}

// The answerer election used to live here: hasAnsweringClient, answererLocked, and isAnswerer, which
// together picked one attached client to answer terminal queries directly and dropped query replies from
// every other client.
//
// All three are gone, and the deletion is the point of the proxy design rather than a side effect. Electing
// an answerer has to be right in four separate situations, and each one was a bug in turn: a read-only
// follower elected, so nothing answered and the querying program hung; a reserved-but-not-yet-attached
// client elected, with the same result, in a window Service.Attach deliberately opens to resize before
// snapshotting; two attached clients both answering, so a single CSI c came back as
// "\x1b[?62;52;c\x1b[?62;52;c"; and across a server restart cm answering a query from the backlog that the
// reconnecting client then answered again from the log, which typed a git branch name into the prompt.
//
// cm is now the only writer of a reply to a pty. It answers what its terminal model can answer, and for a
// query only a real terminal can answer it asks one client and relays what comes back, matching the reply
// to the request. There is no election because there is nothing to elect: a client is a source cm consults,
// never an answerer. See internal/server/queryproxy.go, and docs/architecture.md for the comparison with
// tmux, which works this way and does not have this family of bug.

// EverWatched reports whether a client has attached in order to display this session.
//
// Monotonic, unlike the client count, and that is what makes it usable when a session ends: a client
// detaching as its shell exits is the normal shape of typing `exit` in a window, so the count is already
// zero by the time anything asks, while "was someone watching this" stays true.
//
// Counting attachments alone was too loose. `cm attach --no-attach` and `cm run` both open an attachment
// and immediately detach, purely to create the session, so every session looked watched -- and a `cm run`
// task was then forgotten along with the exit status that was its entire product.
//
// Keyed on whether the client wanted a screen repaint, which is what actually separates the two. A client
// painting a terminal needs the session's current screen; every caller that is only creating a session,
// streaming bytes, or following output sets NoRestore because a repaint would duplicate or corrupt what it
// is writing. So "asked to be shown the screen" means "there is a terminal displaying this".
func (s *Session) EverWatched() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.watched
}

// noteWatched records that a client attached to display the session.
func (s *Session) noteWatched() {
	s.mu.Lock()
	s.watched = true
	s.mu.Unlock()
}

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
	reader *seqlog.Reader[seq.Log]
	// restore holds bytes reproducing the current screen, empty when resuming.
	restore []byte
	// first reports that this is the only attached client, so a program that tracks focus should
	// be told someone is watching again.
	first bool
	// evict is closed when something outside this client asks it to detach, which is what `cm detach`
	// does. The attach loop selects on it and returns as though the user had pressed the detach key.
	//
	// A channel rather than a flag, because the loop it has to reach is blocked in a select waiting on
	// output that a quiet session may not produce for hours. A flag would only be noticed on the next
	// byte, so detaching an idle session would appear to hang.
	evict chan struct{}
	// queries carries questions cm needs this client's terminal to answer, such as the background colour
	// or the clipboard. The attach loop forwards them to the client, which writes them to its terminal.
	//
	// Separate from the output stream because these are addressed to one client rather than to everything
	// watching the session. Sending a question through the session's output would ask every attached
	// terminal, and each would answer, which is the duplicate-reply bug again; it would also write into
	// the session's scrollback a question the shell never asked.
	queries chan []byte
}

// reserveClient registers a client for sizing before it has attached, returning its token.
//
// This exists so the caller can settle the session's size *before* the screen is serialized. The
// sizing policy needs a token to decide with, and the token used to come only from attach, which
// forced the resize to happen after the snapshot and reintroduced a bug that had already been fixed
// once: a screen serialized at the old width, and a shell redraw generated after those bytes were
// taken. See Service.Attach for the two symptoms.
//
// The reservation is discarded by detach along with everything else keyed by the token, so a caller
// that reserves and then fails to attach leaves nothing behind as long as it releases it.
func (s *Session) reserveClient() *attachToken {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reserveClientLocked()
}

// reserveClientLocked allocates the token and its size entry.
//
// Split out because attach also needs it while already holding mu, and the ordering of the counter
// and the map entry must be identical on both paths.
func (s *Session) reserveClientLocked() *attachToken {
	s.attachOrder++
	token := &attachToken{order: s.attachOrder}
	s.clientSizes[token] = &clientSize{order: s.attachOrder}
	return token
}

// releaseClient drops a reservation that never became an attachment.
//
// Without it a client whose attach failed between reserving and attaching would leave a size entry
// behind, and under ResizeLeader that entry is enough to hold sizing: the session would keep sizing
// itself to a window that no longer exists.
//
// Deliberately not releaseClientSize, which is the detach path and recomputes the session's size so
// it can grow once a constraint leaves. Nothing is ever displayed for a reservation that failed, so
// there is no size to give back: resizing the pty here would make the shell redraw for a client that
// never arrived, and under ResizeSmallest it would resize the session twice for one failed attach.
func (s *Session) releaseClient(tok *attachToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.clientSizes, tok)
	if s.leader == tok {
		s.leader = nil
	}
}

// attach registers a subscriber and returns it along with the sequence number its stream
// begins at.
//
// tok is a token from reserveClient, so the caller can size the session before the screen is taken.
// Passing nil allocates one here, which is what a caller with no sizing to do wants.
//
// The caller gets restore bytes and a starting sequence under one lock, so no output can
// slip between snapshotting the screen and subscribing. Getting that wrong would show a
// client a screen that is either missing bytes or replaying them twice.
func (s *Session) attach(resumeFrom *seq.Log, tok *attachToken) (attachment, error) {
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
		return s.newAttachmentLocked(s.recent.Next(), restore, tok), nil
	}

	if resumeFrom != nil {
		from = *resumeFrom
	} else if s.term != nil {
		// The screen and the position it corresponds to are read together, under termMu, so the pump
		// cannot advance the model between the two.
		//
		// Taken while holding mu, which is the only permitted order. feedTerminal deliberately drops
		// mu before acquiring termMu so this cannot deadlock against it.
		s.termMu.Lock()
		b, err := s.term.Restore()
		// Backed off to the last complete sequence, so a partial one is replayed rather than lost. Clamped
		// at the log's start, which matters only for a session whose very first bytes are a partial
		// sequence, where the backlog can be the whole of what the model has seen.
		modelEnd := s.modelSeq
		if pending := seq.Log(s.modelPending); pending <= modelEnd {
			modelEnd -= pending
		}
		s.termMu.Unlock()
		if err != nil {
			return attachment{}, fmt.Errorf("serializing terminal state: %w", err)
		}

		// Images are re-transmitted ahead of the screen, and the order is the whole of it: the restored
		// screen may contain placements referring to images by id, and a placement whose image the
		// terminal has never seen draws nothing. Sending the images first means the ids resolve.
		//
		// The payloads are the ones the program sent, replayed verbatim, so this costs what the original
		// transmission cost. Rebuilding them from libghostty's storage instead would mean re-encoding
		// decoded pixels, measured at 90x the inbound size, 11815084 bytes against 217378 for one
		// screenshot.
		//
		// Every command is forced to q=2 by the store, so a re-transmission generates no response. That
		// is what keeps this off the reply path: an image cm sends asks the terminal nothing, so nothing
		// comes back to be mistaken for an answer to a question cm never asked.
		restore = append(s.graphicsRestore(), b...)

		// State is replayed, so streaming starts where the replayed screen ends rather than repeating
		// history the snapshot already covers.
		//
		// Where the *model* ended, not the log's end, and the difference is output rather than
		// duplication. The pump wakes clients before feeding the model, so the log can be ahead of the
		// screen just serialized; starting at the log's end would skip exactly that gap, and the
		// client would never see those bytes from anywhere. Starting at the model's end replays them,
		// which is correct because the snapshot does not contain them.
		//
		// The model's last sequence *boundary* rather than its raw position, which is the other half of
		// the same problem. A model fed a partial sequence holds it in its parser, and the screen the
		// snapshot serializes cannot express that, so resuming at the raw position drops those bytes from
		// both halves: a client saw the title set and `:2:3m` arrive as text with the nine bytes that
		// opened the SGR missing. Resuming at the boundary replays the partial sequence, which costs a
		// few duplicated bytes the terminal parses correctly. See modelBoundary.
		//
		// Both are positions in the log's numbering, not lastSeq's. lastSeq counts the shim's bytes
		// while the log numbers the rewritten ones, and prompt rewriting makes those differ by however
		// much it added. Using lastSeq here starts the stream at an offset inside an escape sequence,
		// which slices the ESC off and leaves a cursor move rendering as literal text beside the
		// prompt.
		from = modelEnd
	}

	return s.newAttachmentLocked(from, restore, tok), nil
}

// SubscribeOutput follows the session's output from its current position.
//
// Distinct from attaching, which also registers a client for sizing, counts toward the session's client
// total, and can restore a screen. A caller that only wants to observe bytes -- a match wait, and later a
// watch -- must do none of those: registering as a client would make a wait for output change which
// terminal owns the session's size, and would report a session as attached when nothing is watching it.
//
// From the current end rather than from the oldest retained byte, because a wait asks about what happens
// next. Starting from history would satisfy a wait for text the session printed before the caller asked,
// which is the same mistake as a wait satisfied by the state a session was already in.
func (s *Session) SubscribeOutput() *seqlog.Reader[seq.Log] {
	_, next := s.recent.Bounds()
	return s.recent.Subscribe(next)
}

// newAttachmentLocked builds an attachment and registers it for sizing.
//
// One place rather than at each return, because there are two paths out of attach and the earlier
// version registered on only one of them, so a client restored from disk had no size entry and could
// never own sizing.
// tok is the reservation from reserveClient when the caller made one, so the size entry it already
// holds is kept rather than replaced: replacing it would discard the size the session was just resized
// to and, under ResizeLeader, the leadership that went with it.
func (s *Session) newAttachmentLocked(from seq.Log, restore []byte, tok *attachToken) attachment {
	token := tok
	if token == nil {
		token = s.reserveClientLocked()
	}

	// Marked attached only now, which is what makes this client eligible to answer a terminal query.
	// Until this point it is a reservation holding a size, with no stream to receive a query on and no
	// input channel to reply from, and electing one leaves a query answered by nobody. Set here rather
	// than in attach because both paths out of attach come through this function, which is the same
	// reason the size entry is registered here.
	if cs := s.clientSizes[token]; cs != nil {
		cs.attached = true
		// Stamped here for the same reason attached is: this is the moment the attachment becomes live,
		// and both paths out of attach come through here. Reported only.
		cs.attachedAt = s.now()
		// Whether this client is starting out with no main-screen content, for the same reason: both paths
		// come through here, and the first attempt set it in attach's snapshot branch only, which the
		// resume path and the stored-restore path both skip.
		//
		// A client attaching while a full-screen program runs gets a blob describing the alternate screen,
		// and nothing describes the main one, because libghostty serializes the active screen and offers no
		// way to reach the other. So its terminal's main screen holds whatever that window held before, and
		// the ?1049l the program sends on the way out pops it onto that. See markAltScreenLeft.
		if s.term != nil {
			if onAlt, err := s.term.OnAltScreen(); err == nil && onAlt {
				cs.openedOnAlt = true
			}
		}
	}

	evict := make(chan struct{})
	// Lazily created, so a Session built field by field rather than through newSession still attaches.
	// Several tests construct one to hold an exact intermediate state, and a nil map here panics only on
	// attach, which is a long way from the missing line that caused it.
	if s.evicts == nil {
		s.evicts = make(map[*attachToken]chan struct{})
	}
	s.evicts[token] = evict

	// Buffered, so markAltScreenLeft never blocks on a client that is between selects, and depth one
	// because two pending repaints are the same repaint.
	repaint := make(chan struct{}, 1)
	if cs := s.clientSizes[token]; cs != nil {
		cs.repaint = repaint
	}

	// The channel carrying questions cm needs this client to answer. Buffered so the output pump is never
	// blocked behind a client's socket: a full buffer costs one unanswered query, where blocking would
	// stall every session's output behind the slowest terminal.
	queries := make(chan []byte, 8)
	if s.queries == nil {
		s.queries = make(map[*attachToken]chan []byte)
	}
	s.queries[token] = queries

	return attachment{
		token:   token,
		reader:  s.recent.Subscribe(from),
		restore: restore,
		first:   s.clients.Add(1) == 1,
		evict:   evict,
		queries: queries,
	}
}

// markReadOnly records that an attachment is a follower, so it is not mistaken for a client that
// can answer a terminal query.
//
// Separate from registerClientSize, which learns the same flag, because that is skipped entirely for
// a resuming client: a read-only follower that reconnected would otherwise look like an answerer and
// leave a querying program waiting forever. Called immediately after attach, before any output is
// pumped, so drainPending never sees a follower counted as an answerer.
func (s *Session) markReadOnly(tok *attachToken) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cs := s.clientSizes[tok]; cs != nil {
		cs.readOnly = true
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

// EvictClients asks every attached client to detach, returning how many were asked.
//
// The session keeps running. That is the whole point: this is a detach rather than a kill, so the attach
// loop treats an eviction exactly as it treats the detach key.
//
// Counted rather than reported as a bool because a session can hold several clients, and a caller
// clearing a session wants to know whether one window or four just let go. Zero is a normal answer: a
// session with nothing attached is already in the requested state.
//
// Idempotent. A second call while the first eviction is still in flight finds the channel already
// closed and skips it, so a retry cannot panic on a double close.
func (s *Session) EvictClients() int {
	asked, _ := s.evictClients(false, "", false, "")
	return asked
}

// UpgradeClients asks attached clients to re-exec and come back, returning how many were asked and how
// many were left alone for already running current.
//
// Unlike EvictClients this preserves the attachment: the client is expected to replace itself with the
// newest binary and reattach from where it left off, which it already knows how to do because that is
// what it does across a server restart.
//
// current is the build to compare against, and a client already running it is skipped unless force is
// set. That keeps the command idempotent: running it twice after one upgrade does not make every window
// repaint for nothing. A client that reported no version is never skipped, since an unknown build is
// more likely to be old than current.
//
// activeOnly upgrades just the client someone is using, leaving every other window attached to the same
// session alone. Zero asked when no active client can be named, rather than upgrading all of them.
func (s *Session) UpgradeClients(force bool, current string, activeOnly bool) (asked, alreadyCurrent int) {
	return s.evictClients(true, map[bool]string{true: "", false: current}[force], activeOnly, "")
}

// SwitchClients asks clients to come back attached to another session, named by ref.
//
// The same in-place replacement an upgrade uses, which is what keeps the window from resetting: the
// client execs over itself without restoring the terminal, so the screen goes straight from one session
// to the other. Zero asked when activeOnly is set and no client can be named as the active one.
func (s *Session) SwitchClients(ref string, activeOnly bool) (asked int) {
	asked, _ = s.evictClients(true, "", activeOnly, ref)
	return asked
}

// evictClients closes each client's eviction channel, optionally marking the request as an upgrade.
//
// Shared by both callers so the bookkeeping cannot drift: the upgrade flag has to be recorded before the
// channel is closed, since closing it is what wakes the streaming loop that reads the flag.
//
// skipVersion names a build to leave alone, or is empty to ask everyone. Only the upgrade path passes
// one; a detach applies to every client regardless of what it is running.
//
// activeOnly restricts the eviction to the client someone is using. With no active client to name,
// nothing is asked and nothing is skipped: the caller asked for one specific window and cm does not know
// which it is, so acting on all of them would be the opposite of the request.
func (s *Session) evictClients(
	upgrade bool, skipVersion string, activeOnly bool, switchTo string,
) (asked, skipped int) {
	s.mu.Lock()
	defer s.mu.Unlock()

	var only *attachToken
	if activeOnly {
		only = s.activeClientLocked()
		if only == nil {
			return 0, 0
		}
	}

	for tok, ch := range s.evicts {
		if only != nil && tok != only {
			// Not counted as skipped. That count means "left alone for already being current" and is
			// reported as such, so folding in the clients this flag was never going to touch would tell a
			// caller that windows were up to date when nothing looked at their version.
			continue
		}
		if skipVersion != "" {
			// Compared against what the client reported. An empty version is never skipped: a client too
			// old to say is more likely to be an old build than a current one, and asking it costs a
			// repaint while wrongly skipping it leaves the stale client in place, which is the failure
			// this command exists to fix.
			if cs := s.clientSizes[tok]; cs != nil && cs.version == skipVersion {
				skipped++
				continue
			}
		}
		select {
		case <-ch:
			// Already evicted and not yet torn down, so it is not asked twice and not counted twice.
		default:
			// Recorded before the close, which is what wakes the loop that reads it. Setting it after
			// would race the streaming loop and send an ordinary detach, making the client exit instead
			// of coming back: a window closing rather than upgrading.
			if upgrade {
				if s.upgrading == nil {
					s.upgrading = make(map[*attachToken]bool, 1)
				}
				s.upgrading[tok] = true
			}
			// Recorded before the close for the same reason as the upgrade flag: closing the channel is
			// what wakes the loop that reads this, so setting it afterwards would race that loop and send
			// a bare upgrade, which reattaches to the session the client is leaving.
			if switchTo != "" {
				if s.switching == nil {
					s.switching = make(map[*attachToken]string, 1)
				}
				s.switching[tok] = switchTo
			}
			close(ch)
			asked++
		}
	}
	return asked, skipped
}

// isUpgrading reports whether this attachment's eviction was an upgrade request.
//
// Read by the streaming loop once its eviction channel closes, to decide which kind of Detached to
// send: one the client exits on, or one it comes back from.
func (s *Session) isUpgrading(tok *attachToken) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upgrading[tok]
}

// switchTarget returns the session reference this attachment was asked to switch to, or empty.
func (s *Session) switchTarget(tok *attachToken) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.switching[tok]
}

// detach releases a subscriber, reporting whether it was the last one.
func (s *Session) detach(a attachment) (last bool) {
	a.reader.Close()
	if a.token != nil {
		// Dropped here rather than in EvictClients, so the channel outlives the eviction it carries: the
		// attach loop has to still be able to select on it after the close, and removing it there would
		// leave a client blocked on a channel nothing holds.
		s.mu.Lock()
		delete(s.evicts, a.token)
		delete(s.queries, a.token)
		// Dropped with the channel it describes. Left behind, an entry would accumulate for the life of
		// the session, and since tokens are pointers a later one could reuse the address and be treated
		// as an upgrade it never asked for.
		delete(s.upgrading, a.token)
		delete(s.switching, a.token)
		// Any question still outstanding with this client will never be answered now, so it is released
		// rather than left to expire. Waiting for the timeout would hold every reply queued behind it for
		// up to requestTimeout after the client that could have answered has gone.
		for _, r := range s.requests {
			if r.proxied && r.tok == a.token {
				r.proxied = false
				r.data = nil
			}
		}
		ready := s.takeReadyLocked()
		s.mu.Unlock()
		s.writeReplies(ready)

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

// terminalModes reports which optional terminal modes a program in the session has enabled.
//
// Used to recognize an event generated for a program that has since exited. A session with no model,
// which is any session whose terminal failed to start, reports both modes on, so nothing is ever dropped
// on the strength of state cm does not have.
func (s *Session) terminalModes() input.TerminalModes {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return input.TerminalModes{KittyKeyboard: true, FocusReports: true}
	}
	return input.TerminalModes{
		KittyKeyboard: term.KittyKeyboardProtocol(),
		FocusReports:  term.FocusReporting(),
	}
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
	// The write that cannot otherwise be made to fail: os.File.Write loops until the pty accepts
	// everything, so a short or failed write has no natural cause to provoke.
	if err := fault.Err(fault.BeforeShimWrite); err != nil {
		return err
	}
	_, err := s.shim.Write(ctx, &shimv1.WriteRequest{Data: data})
	return err
}

// Signal delivers a signal to the session's shell, and to its process group unless processOnly.
//
// The group by default, because that is what a keypress does: a pty delivers to the foreground process
// group, so signalling only the shell would leave the job the caller meant to stop still running.
func (s *Session) Signal(ctx context.Context, sig int32, processOnly bool) error {
	_, err := s.shim.Signal(ctx, &shimv1.SignalRequest{
		Signal:       sig,
		ProcessGroup: !processOnly,
	})
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
		s.reportSize(uint16(rows), uint16(cols))
	}
	return nil
}

// reportSize tells a program that asked for in-band size reports that the size changed.
//
// The pty ioctl above is not enough on its own. A program that enables mode 2048 stops acting on
// SIGWINCH and waits to be told in band, so without this it keeps drawing at the old size no matter how
// many times the kernel signals it. Measured: nvim held 30x100 through four consecutive resizes while
// the pty correctly reported 14x99, 9x89, 15x109, and 11x79 in turn, and a single hand-fed report moved
// it immediately.
//
// After the ioctl rather than before, so the program cannot read a size the pty does not have yet: a
// report is an invitation to call TIOCGWINSZ, and nvim does exactly that. Ordering it the other way
// makes the report a lie for as long as the two calls are apart.
//
// Sent through the same queue as every other reply, so it cannot overtake an outstanding question. A
// report arriving out of turn is consumed as the answer to whatever was asked before it, which is the
// recorded `wallfacer -h` corruption, and this is a reply the program did not ask for at a moment it may
// well be mid-query.
//
// Failures are logged rather than returned. The resize itself has already succeeded at the only layer
// that must not fail, and refusing a resize because a capability query failed would trade a stale
// repaint for a broken session.
func (s *Session) reportSize(rows, cols uint16) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return
	}
	report, err := term.SizeReport(rows, cols)
	if err != nil {
		s.log.Warn("reading the in-band resize mode failed, so no size report was sent",
			"session", s.label, "error", err)
		return
	}
	if len(report) == 0 {
		return
	}
	s.queueOrWriteReply([][]byte{report})
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

// ReadVT returns the last lines of the session's output as escape sequences.
//
// The raw counterpart of Read, so `cm read --raw` can bound its output the way the plain form does. `cm history
// --format vt` renders the whole scrollback and has no line limit, which is the gap this fills.
func (s *Session) ReadVT(lines int) ([]byte, error) {
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return nil, nil
	}
	return term.TailVT(lines)
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
func (s *Session) Shutdown(ctx context.Context, force bool, sig int32) (surviving []int32, err error) {
	resp, err := s.shim.Shutdown(ctx, &shimv1.ShutdownRequest{Force: force, Signal: sig})
	if err != nil {
		return nil, err
	}
	// Empty from an older shim as well as from a clean shutdown, which the proto notes. That ambiguity
	// only ever costs a warning, so it is not worth a capability probe.
	if len(resp.SurvivingPids) > 0 {
		s.log.Warn("processes survived the shutdown signal",
			"pgid", resp.SignalledPgid, "surviving", resp.SurvivingPids)
	}
	return resp.SurvivingPids, nil
}

// State queries the shim directly, which is the authority on whether a session is alive.
func (s *Session) State(ctx context.Context) (*shimv1.StateResponse, error) {
	return s.shim.State(ctx, &shimv1.StateRequest{})
}

// setLabel records a name to call this session by in messages and logs.
//
// Last one wins, which is right for a session with several names: whichever the caller most recently
// reached it by is the one they are thinking in.
func (s *Session) setLabel(name string) {
	if name == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.label = name
}

// ID returns the session's identity.
func (s *Session) ID() string { return s.id }

// Label returns what to call this session when talking to a person.
func (s *Session) Label() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.label
}
