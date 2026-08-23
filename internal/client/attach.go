package client

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

const (
	// reconnectInterval is how often to retry while the server is away, for as long as the outage
	// looks like an ordinary restart.
	reconnectInterval = 100 * time.Millisecond
	// reconnectSlowInterval is how often to retry once an outage has outlasted
	// reconnectQuietPeriod.
	//
	// Backing off matters because retrying is now unbounded: a client whose server is never coming
	// back would otherwise dial ten times a second for as long as the window stays open. Reached only
	// after the quiet period, so an ordinary restart still reconnects at the fast interval and pays
	// nothing for this.
	reconnectSlowInterval = 1 * time.Second
	// reconnectQuietPeriod is how long an outage may last before it is worth mentioning.
	//
	// A restart takes about 450ms (docs/architecture.md), so anything under a few seconds is the
	// expected case and logging it is noise: a user restarting the server would get a line per session
	// per restart, which is what made the log useless for finding a real outage. Past this the outage
	// is no longer routine and silence is the worse failure, since the client holds the terminal and a
	// wait looks identical to a hang.
	reconnectQuietPeriod = 3 * time.Second
	// reconnectLogInterval is how often to repeat that an outage is still going.
	//
	// Repeated rather than logged once, so a session that has been waiting ten minutes says so instead
	// of leaving a single stale line from the moment it started waiting.
	reconnectLogInterval = 30 * time.Second
	// inputReadSize bounds a single read of keystrokes. Human input is small; this only
	// needs to cover a paste arriving in one read.
	inputReadSize = 4096
)

// Open builds the Open message these options describe.
//
// Exists because there is more than one way to create a session -- an interactive attach, and
// `attach --no-attach`, which has no terminal to wire up and so does not go through Attach at all --
// and each used to build its own Open. Nothing made the two agree, so a field added to one was
// silently missing from the other: that is how `--tag` came to work everywhere except
// `--no-attach`, where the tags were accepted, validated, and then dropped on the floor.
//
// The fields left unset are the ones a caller must supply because only it knows them: Rows and Cols
// come from a terminal or a convention, and ResumeFromSeq only means anything to a reconnecting
// client.
func (o Options) Open(session string) *serverv1.Open {
	return &serverv1.Open{
		Session:       session,
		ReadOnly:      o.ReadOnly,
		Command:       o.Command,
		Cwd:           o.Dir,
		Env:           o.Env,
		ClientEnv:     o.ClientEnv,
		Persist:       o.Persist,
		OnRestore:     o.OnRestore,
		Tags:          o.Tags,
		NoRestore:     o.NoRestore,
		InsideSession: o.InsideSession,
		// Set here rather than by each caller, which is the whole point of this constructor: a field
		// added to one Open and missed on another is how --tag came to work everywhere except
		// --no-attach. Every client describes itself the same way as a result.
		ClientVersion: paths.Version(),
		ClientPid:     int32(os.Getpid()),
	}
}

// Options configures an attachment.
type Options struct {
	// SocketPath is the server's socket.
	SocketPath string
	// Session to attach to. Empty asks the server to allocate a name.
	Session string
	// ReadOnly follows the session without sending input.
	ReadOnly bool
	// Command overrides the shell when creating.
	Command []string
	// Dir is the working directory when creating.
	Dir string
	// Env holds extra KEY=VALUE entries when creating.
	Env []string
	// ClientEnv holds terminal-related variables from this client's environment, recorded by the
	// server so a shell inside the session can refresh them later.
	ClientEnv map[string]string
	// Persist requests that this session's content survive a reboot.
	Persist bool
	// OnRestore overrides the configured restore behavior for this session.
	OnRestore string
	// Tags label a session being created, for grouping and filtering. Ignored when attaching to a
	// session that already exists, since an attach is not how tags are changed.
	Tags map[string]string
	// DetachKey is the key that detaches. Zero value means the default.
	DetachKey DetachKeySpec
	// NoRestore skips the screen repaint that normally opens an attachment, streaming only what arrives from
	// now on.
	//
	// For a follower that is not painting a terminal, where the repaint duplicates output the caller has
	// already printed.
	NoRestore bool

	// InsideSession names the cm session this client is itself running in, when it is running in one.
	//
	// Tells the server that this attachment is nested, so the parent session stops attributing the
	// bytes passing through it to itself. See Session.beginHosting for why the server cannot work
	// that out alone.
	//
	// Set by the caller rather than read here, so the one place that consults the environment is the
	// command layer and this package stays a library. InsideCmSession is the helper that decides it.
	InsideSession string

	// Output, when set, receives the session's bytes instead of the terminal.
	//
	// For a caller that is not painting a terminal and wants to transform what it writes -- stripping escape
	// sequences, say. The terminal is still opened, since the attachment needs its size and stdin, but nothing
	// is written to it.
	//
	// A hook rather than letting the caller pass its own writer to OpenTTY, because a TTY needs an *os.File
	// for the ioctls that report size and set raw mode, and a filter is not a file.
	Output io.Writer

	// Log records what the client did, for diagnosing an attachment that misbehaved.
	//
	// Both Attach and runSession install a discarding logger when one is not supplied, so no call site needs a
	// nil check. Defaulted in both rather than only in Attach because runSession is driven directly by its
	// tests, where a nil logger was a panic in whichever branch happened to log rather than a missing line.
	//
	// A client has diagnostics nothing else can see -- how often it reconnected, where it resumed from, input
	// it had to hold across an outage -- and until this existed they were invisible.
	Log *slog.Logger

	// OnAttached, when set, is called once the server has opened the session on this connection.
	//
	// A reliable readiness signal, which OnMetadata is not: metadata is delivered when the session reports a
	// title or directory, so a quiet session never fires it. Anything that has to order an action after the
	// attachment is live -- sending input, say -- needs this instead. Called on every connection, so a
	// reconnect signals again.
	OnAttached func()

	// OnMetadata, when set, is called as the session reports its title and directory.
	//
	// This is how a terminal emulator learns values the shell reported to cm rather than to the
	// terminal, so it can retitle a tab or open a new window in the right place.
	OnMetadata func(SessionMetadata)

	// ResumeFrom, when set, starts this attachment as a resume rather than a fresh one.
	//
	// For a client replacing one that was asked to upgrade: the terminal already shows the session, so it
	// wants the bytes it missed rather than a repaint. Without it an upgrade would clear and redraw the
	// screen, which is exactly the visible seam the feature exists to avoid, and would also lose the
	// scrollback the terminal holds above the current screen.
	//
	// Distinct from the reconnect position the loop maintains internally, which describes an outage this
	// process lived through. This one is inherited across an exec, so it cannot be discovered locally.
	ResumeFrom *uint64

	// OnOutput, when set, is called after each chunk of output is written, with the position one past its
	// last byte.
	//
	// Exists so a follower can tell when it has caught up to a position it learned elsewhere. `cm send
	// --follow` knows from the wait's reply how far the server had consumed, but not how much of that has
	// reached it; without this it stopped following as soon as the wait returned and truncated whatever was
	// still in flight, losing the command's output about a third of the time.
	OnOutput func(next uint64)
}

// SessionMetadata is what a session reports about itself.
type SessionMetadata struct {
	Title string
	// Cwd is the decoded working directory, empty if the shell has not reported one.
	Cwd string
	// CwdIsLocal is false once a session has ssh'd elsewhere, in which case acting on Cwd
	// locally is wrong.
	CwdIsLocal bool
}

// Result describes how an attachment ended.
type Result struct {
	// Session is what to call the session when talking to the user: the name it was attached by, or its
	// ID with the sigil when it has no name. What "detached from ..." prints.
	Session string
	// SessionID is the session's identity, and what any later call about this session should use.
	//
	// Distinct from Session because a name is a binding: it can be pointed at a different session while
	// this client is attached, so reconnecting by name could land somewhere else. Reconnecting by ID
	// either finds the same session or fails, and failing is the right outcome for a client whose
	// session is gone.
	SessionID string
	// Detached is true when the user detached rather than the session ending.
	Detached bool
	// SwitchTo is the session reference the server asked this client to show instead, or empty.
	//
	// Always empty by the time Attach returns: a switch is handled inside the reconnect loop, so a caller
	// never sees one in progress. It lives here because the loop needs it out of runSession, which reads
	// the server's event, and because a test can then assert what was asked for.
	SwitchTo string
	// Upgrade is true when the server asked this client to come back on a newer build rather than
	// exit. Detached is also set, since an upgrade is a detach that returns.
	//
	// Acted on by the caller rather than here, because replacing the process needs the argv this one
	// was started with and the terminal it owns, neither of which belongs to this package.
	Upgrade bool
	// ResumeFrom is how far output had been consumed when the attachment ended, so a replacement
	// process can pick up exactly there instead of repainting.
	//
	// Only meaningful with Upgrade. Nil when the client never got far enough to have a position, in
	// which case a replacement does an ordinary fresh attach.
	ResumeFrom *uint64
	// Exited is true when the session's shell exited.
	Exited   bool
	ExitCode int
}

// Attach connects a terminal to a session and runs until detach or session end.
//
// The terminal is put into raw mode once and restored once, around the whole attachment
// including reconnects. That is deliberate: a server restart should look like the session
// briefly freezing, not like the client exiting and starting again.
func Attach(ctx context.Context, tty *TTY, opts Options) (Result, error) {
	var result Result
	result.Session = opts.Session

	// Defaulted here rather than at each use, so nothing has to nil-check a logger.
	if opts.Log == nil {
		opts.Log = slog.New(discardLogHandler{})
	}
	log := opts.Log.With("session", opts.Session)

	// resumeFrom tracks how far output has been consumed, so a reconnect asks for exactly
	// what was missed instead of a fresh repaint.
	//
	// Seeded from the options when this process is replacing one that was asked to upgrade. Its terminal
	// still shows the session, so the first attach is a resume rather than a repaint, which is what makes
	// an upgrade look like a freeze instead of a redraw.
	resumeFrom := opts.ResumeFrom

	// Keystrokes typed while the server is away are held here and flushed on reconnect, so
	// a freeze does not silently swallow input.
	var pending []byte

	// SIGWINCH is the only way a client learns its terminal was resized.
	winch := make(chan os.Signal, 1)
	signal.Notify(winch, syscall.SIGWINCH)
	defer signal.Stop(winch)

	// Reading the terminal must not block the reconnect loop, so input is read once by a
	// long-lived goroutine and delivered over a channel. A read blocked in the kernel
	// cannot be cancelled, which is why this goroutine outlives individual connections.
	input := make(chan []byte, 16)
	inputErr := make(chan error, 1)
	go readInput(tty, input, inputErr)

	// outage tracks the current disconnection: when it started, whether it has been reported, and
	// whether this client has ever been connected at all.
	//
	// Retrying is deliberately unbounded, and replaces a 30s budget that killed real sessions. That
	// budget was set on a client's *first* failure and never reset on a successful reconnect, so it was
	// a limit on a client's whole lifetime rather than on one outage: a window open for hours had
	// already spent it on earlier restarts, and the next restart closed it. Observed with three
	// sessions dying together while every other session reconnected, the difference being only that
	// those three had first reconnected hours earlier.
	//
	// Unbounded is also the right behavior independent of that bug. The shim holds the pty and the shell
	// keeps running, so a server that is slow to come back is a reason to wait, not a reason to discard
	// a terminal someone is using. The cancel signal is ctx, which a closing window delivers, so
	// nothing waits forever against the user's wishes.
	// ref is what each attempt asks for. It starts as whatever the caller gave, so the first Open is the
	// request the user made, and becomes an ID or a switch target as the loop learns better.
	ref := opts.Session

	var outage outageState
	for {
		conn, cl, err := dial(opts.SocketPath)
		if err != nil {
			// A first attempt that cannot reach the server is a hard failure: there is no session on
			// screen to preserve and no reason to think one is coming. Once connected, the same error
			// means an outage, which is waited out however long it takes.
			if !outage.everConnected {
				return result, fmt.Errorf("connecting to server: %w", err)
			}
			outage.begin(err, resumeFromValue(resumeFrom), len(pending))
			outage.report(log)
			if waitErr := outage.sleep(ctx); waitErr != nil {
				return result, waitErr
			}
			continue
		}
		log.Debug("connected to the server", "resume_from", resumeFromValue(resumeFrom))
		// Reached the server, so any outage is over. Logged only if it was reported, which keeps an
		// ordinary restart silent on both sides rather than announcing a recovery nobody was told about.
		outage.end(log)

		outcome, err := runSession(
			ctx, tty, cl, opts, ref, &result, &resumeFrom, &pending, winch, input, inputErr)
		conn.Close()

		switch outcome {
		case outcomeSwitch:
			// The same process, the same terminal, the same input goroutine: only the session changes.
			// Nothing is restored or re-rawed, so the screen goes straight from one session to the other,
			// and `ps` keeps showing the command this window was started with.
			ref = result.SwitchTo
			// The position belongs to the session being left, and two sessions do not share a numbering,
			// so the new one is repainted from its own screen rather than resumed.
			resumeFrom = nil
			// Input typed before the switch was meant for the session being left. Replaying it into the
			// target would type someone else's keystrokes into another shell, so it is dropped, which is
			// what replacing the process would have done anyway.
			pending = pending[:0]
			// None of these describe the attachment any more: it did not end, it is not upgrading, and
			// the target has been consumed. Cleared so a caller cannot see a half-finished switch, and so
			// "detached from ..." is not printed for a window that is still on screen.
			result.Detached, result.Upgrade, result.SwitchTo = false, false, ""
			log.Debug("switching this client to another session", "session", ref)
			continue
		case outcomeDone:
			// Carried out so a replacement process can resume exactly here rather than repainting. Set
			// only for an upgrade: every other way of finishing is a client that is not coming back, and
			// handing out a position would invite a caller to reattach after a session ended.
			if result.Upgrade {
				result.ResumeFrom = resumeFrom
			}
			return result, err
		case outcomeReconnect:
			// A session that was never opened, refused for a reason of its own, is not something to retry.
			//
			// Retrying was silent and forever: `cm attach @deadbeef` printed nothing and sat in this loop,
			// because an ID that resolves to nothing is refused by the server rather than created, unlike a
			// name. A typo therefore hung a window with no message. Reachable only since references could
			// name an identity; before that every reference either found a session or made one.
			//
			// Scoped by both conditions rather than one. Never-opened alone would break a first attach that
			// raced a server restart, where the Open fails because the server is going away and the retry
			// is what makes it work. A closed transport is that case, and it is the same distinction
			// `cm server restart` and `cm kill` already draw on. Once a session has been opened, every
			// failure is an outage and is waited out however long it takes, which is the behavior that
			// keeps a window alive across a restart.
			if result.SessionID == "" && err != nil && !transport.IsClosed(err) {
				return result, err
			}
			// By ID from here on, which is what makes a reconnect a return to one particular session
			// rather than a fresh request for whatever a name points at now.
			if result.SessionID != "" {
				ref = paths.FormatSessionID(result.SessionID)
			}
			outage.begin(err, resumeFromValue(resumeFrom), len(pending))
			outage.report(log)
			if waitErr := outage.sleep(ctx); waitErr != nil {
				return result, waitErr
			}
		}
	}
}

// outageState tracks one disconnection, so the reconnect loop can wait indefinitely without either
// logging every routine restart or going silent through a long outage.
//
// A struct rather than a handful of locals because the two call sites in the loop, a failed dial and a
// dropped session, have to treat an outage identically. They did not before: only one of them logged,
// so a client that could not dial at all left nothing in the log, which is exactly the case that
// needed explaining.
type outageState struct {
	// everConnected distinguishes "never reached the server" from "lost the server", which get
	// opposite treatment: the first fails immediately, the second waits.
	everConnected bool
	// since is when the current outage began, zero when connected.
	since time.Time
	// reported is whether this outage has been logged, so recovery is announced only if the problem
	// was.
	reported bool
	// lastLogged is when this outage was last mentioned, for the periodic reminder.
	lastLogged time.Time
	// err, resumeFrom, and pending describe the outage for the log.
	err        error
	resumeFrom int64
	pending    int
}

// begin records that an outage is in progress, keeping the start time of the first failure.
func (o *outageState) begin(err error, resumeFrom int64, pending int) {
	if o.since.IsZero() {
		o.since = time.Now()
	}
	o.err = err
	o.resumeFrom = resumeFrom
	o.pending = pending
}

// report logs an outage that has lasted long enough to be worth mentioning, and repeats periodically.
//
// Silent below reconnectQuietPeriod, which is the whole point: a server restart takes about 450ms and
// the client recovers by itself, so logging it produced one line per session per restart and buried the
// outages that mattered. Past the quiet period the reasoning inverts, because the client holds the
// terminal while it waits and an unexplained freeze is indistinguishable from a hang.
func (o *outageState) report(log *slog.Logger) {
	if o.since.IsZero() {
		return
	}
	waited := time.Since(o.since)
	if waited < reconnectQuietPeriod {
		return
	}
	if o.reported && time.Since(o.lastLogged) < reconnectLogInterval {
		return
	}

	// Rounded, since the exact microsecond of an outage is noise and a round number is what a reader
	// compares against the restart they just did.
	log.Info("waiting for the server to return",
		"error", o.err, "waited", waited.Round(100*time.Millisecond),
		"resume_from", o.resumeFrom, "pending_bytes", o.pending)
	o.reported = true
	o.lastLogged = time.Now()
}

// end clears the outage, reporting recovery only when the outage itself was reported.
func (o *outageState) end(log *slog.Logger) {
	o.everConnected = true
	if o.since.IsZero() {
		return
	}
	if o.reported {
		log.Info("server returned", "outage", time.Since(o.since).Round(100*time.Millisecond))
	}
	o.since = time.Time{}
	o.reported = false
	o.lastLogged = time.Time{}
	o.err = nil
}

// sleep waits before the next attempt, or returns ctx's error if the client was cancelled.
//
// Backs off once the outage is no longer routine. Since retrying never gives up, a server that is gone
// for good would otherwise be dialled ten times a second for the life of the window; after the quiet
// period the same outage is retried once a second, which still reconnects promptly by human standards.
func (o *outageState) sleep(ctx context.Context) error {
	interval := reconnectInterval
	if !o.since.IsZero() && time.Since(o.since) >= reconnectQuietPeriod {
		interval = reconnectSlowInterval
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(interval):
		return nil
	}
}

type outcome int

const (
	// outcomeDone means the attachment is over: detached, session ended, or a real error.
	outcomeDone outcome = iota
	// outcomeReconnect means the connection dropped but the session may still exist.
	outcomeReconnect
	// outcomeSwitch means the server asked this client to show a different session.
	//
	// Its own outcome rather than a flavour of outcomeReconnect, because the two differ in everything
	// except looping: a reconnect resumes the same session from a recorded position after waiting out an
	// outage, while this attaches to another session immediately, from the top, with the position and any
	// pending input discarded. Sharing the branch would have made a switch wait out a backoff and log an
	// outage that never happened.
	outcomeSwitch
)

// runSession runs one connection's worth of attachment.
func runSession(
	ctx context.Context,
	tty *TTY,
	cl serverv1.ServerClient,
	opts Options,
	// ref is the session this attempt asks for: what the caller typed on the first attach, the session's
	// ID on a reconnect, and the target on a switch. Passed in rather than derived from result, so the
	// loop that knows which of the three this is decides, and this function does not have to infer it.
	ref string,
	result *Result,
	resumeFrom **uint64,
	pending *[]byte,
	winch <-chan os.Signal,
	input <-chan []byte,
	inputErr <-chan error,
) (outcome, error) {
	// Defaulted again rather than relied on from Attach. Attach fills this in, but this function is
	// also driven directly by its tests, which is the whole reason the loop body is separable, and a
	// nil logger there is a panic in whichever branch happens to log rather than a missing line.
	if opts.Log == nil {
		opts.Log = slog.New(discardLogHandler{})
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	stream, err := cl.Attach(ctx)
	if err != nil {
		return outcomeReconnect, err
	}

	rows, cols := tty.Size()
	xpixel, ypixel := tty.PixelSize()
	open := opts.Open(ref)
	open.Rows = uint32(rows)
	open.Cols = uint32(cols)
	open.XPixel = uint32(xpixel)
	open.YPixel = uint32(ypixel)
	open.ResumeFromSeq = *resumeFrom
	if err := stream.Send(&serverv1.AttachRequest{
		Event: &serverv1.AttachRequest_Open{Open: open},
	}); err != nil {
		return outcomeReconnect, err
	}

	// The server's first reply names the session and carries any state to repaint.
	first, err := stream.Recv()
	if err != nil {
		return outcomeReconnect, err
	}
	opened := first.GetOpened()
	if opened == nil {
		return outcomeDone, errors.New("server did not open the session")
	}
	result.Session = opened.Session
	result.SessionID = opened.SessionId
	// Signalled here, after Opened and before anything else is read, so a caller ordering work after the
	// attachment cannot race the first output.
	if opts.OnAttached != nil {
		opts.OnAttached()
	}

	if len(opened.Restore) > 0 {
		if opts.Output != nil {
			// No clear: that writes an escape sequence to a terminal, and a caller taking the bytes itself is
			// not painting one.
			if _, err := opts.Output.Write(opened.Restore); err != nil {
				return outcomeDone, err
			}
		} else {
			// Clear first so restored state is not painted over whatever the client's own
			// shell left on screen.
			if err := tty.Clear(); err != nil {
				return outcomeDone, err
			}
			if _, err := tty.Write(opened.Restore); err != nil {
				return outcomeDone, err
			}
		}
	}
	seq := opened.NextSeq
	*resumeFrom = &seq

	// Flush anything typed while the server was away.
	if len(*pending) > 0 && !opts.ReadOnly {
		if err := stream.Send(&serverv1.AttachRequest{
			Event: &serverv1.AttachRequest_Input{Input: &serverv1.Input{Data: *pending}},
		}); err != nil {
			return outcomeReconnect, err
		}
		*pending = nil
	}

	// Output arrives on its own goroutine so input handling is never blocked behind a
	// slow write to the terminal.
	out := make(chan outMsg, 16)
	go func() {
		defer close(out)
		for {
			resp, err := stream.Recv()
			select {
			case out <- outMsg{resp, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	detachKey := opts.DetachKey
	if detachKey.Name == "" {
		// Zero value means the caller did not configure one.
		detachKey, _ = ParseDetachKey(DefaultDetachKey)
	}

	// The gate buffers a partial detach sequence across reads, so a CSI-encoded detach split between
	// two reads is still recognized rather than forwarded to the shell, and releases it after
	// escapeGrace so a lone escape is not withheld forever.
	gate := &inputGate{key: detachKey}
	// A timer exists only while the gate is holding something. Nil channels block forever, which is
	// what keeps this case out of the select the rest of the time.
	var (
		holdTimer *time.Timer
		holdC     <-chan time.Time
	)
	stopHold := func() {
		if holdTimer != nil {
			holdTimer.Stop()
			holdTimer = nil
			holdC = nil
		}
	}
	defer stopHold()

	// sendInput delivers keystrokes and reports whether the loop should give up on this connection.
	//
	// A closure rather than repeated inline, because there are now two places that send input -- a
	// read, and the grace expiring -- and the error path is the subtle half: bytes lost to a dropped
	// connection have to be carried into the reconnect rather than dropped, or a keystroke vanishes
	// exactly when the server restarts.
	sendInput := func(buf []byte) (reconnect bool) {
		if len(buf) == 0 || opts.ReadOnly {
			return false
		}
		if err := stream.Send(&serverv1.AttachRequest{
			Event: &serverv1.AttachRequest_Input{Input: &serverv1.Input{Data: buf}},
		}); err != nil {
			*pending = append(*pending, buf...)
			return true
		}
		return false
	}

	for {
		select {
		case msg, ok := <-out:
			if !ok {
				return outcomeReconnect, nil
			}
			if msg.err != nil {
				if errors.Is(msg.err, io.EOF) {
					// The server closed the stream cleanly. Without knowing why, assume
					// the server went away and try to resume.
					return outcomeReconnect, nil
				}
				return outcomeReconnect, msg.err
			}
			if ex := msg.resp.GetExited(); ex != nil {
				result.Exited = true
				result.ExitCode = int(ex.ExitCode)
				return outcomeDone, nil
			}
			// An unsolicited Detached means `cm detach` asked this client to let go, so it leaves as
			// though the user had pressed the detach key.
			//
			// Handled here rather than left to fall through, and that is the whole reason this branch
			// exists: the server closes the stream immediately afterwards, and a clean close is otherwise
			// read as the server having gone away, which returns outcomeReconnect. The client would then
			// reattach within a second and silently undo the detach it was just asked to perform.
			//
			// Always unsolicited, since no client asks for an acknowledgement any more: every Detach
			// this client sends sets no_ack, so the server never replies to one.
			if d := msg.resp.GetDetached(); d != nil {
				result.Detached = true
				// An upgrade is a detach that comes back. The re-exec itself is left to the caller, which
				// owns the terminal and the argv this process was started with; reported here so the
				// reconnect loop stops rather than treating the close as an outage.
				result.Upgrade = d.Upgrade
				result.SwitchTo = d.SwitchTo
				if d.SwitchTo != "" {
					// Handled by looping rather than by finishing. Replacing the process would work and
					// buys nothing here: an upgrade has to exec because the point is to run a different
					// binary, while a switch runs the same one against a different session, which this
					// loop already knows how to do because it is what a reconnect is.
					return outcomeSwitch, nil
				}
				return outcomeDone, nil
			}
			if q := msg.resp.GetQuery(); q != nil {
				// The server is asking this terminal a question it cannot answer itself: the background
				// colour, the clipboard, the window's pixel size. Written to the terminal, whose reply
				// travels back on the ordinary input path, where the server matches it to the request.
				//
				// Written to the tty rather than to opts.Output, and skipped entirely when there is no
				// terminal to ask. A caller taking bytes programmatically has no terminal behind it, so a
				// question written into that stream would corrupt what it is collecting and could never be
				// answered. The server's request then expires, which is the same outcome as today.
				if opts.Output == nil && !opts.ReadOnly {
					if _, err := tty.Write(q.Data); err != nil {
						return outcomeDone, err
					}
				}
				continue
			}
			if m := msg.resp.GetMetadata(); m != nil {
				if opts.OnMetadata != nil {
					opts.OnMetadata(SessionMetadata{
						Title:      m.Title,
						Cwd:        m.Cwd,
						CwdIsLocal: m.CwdIsLocal,
					})
				}
				continue
			}
			if o := msg.resp.GetOutput(); o != nil {
				// A gap means bytes before this chunk never reached this client, so the screen it is
				// looking at was built from an incomplete stream. Continuing to write is what makes that
				// visible as corruption: the escape sequences that established the current screen may be
				// part of what was lost, so the next chunk is interpreted against state that never
				// existed. Typically the front of a sequence is missing and its remainder renders as
				// literal text.
				//
				// Repainting is the only recovery, and the server already has the mechanism: a fresh
				// attach answers with a serialized screen. So this drops the resume position and
				// reconnects, which turns the next attach into a fresh one rather than a resume.
				//
				// Only for a client that is painting a terminal. A follower streams bytes to a pipe and
				// sets NoRestore precisely because a repaint would corrupt what it is writing, so for one
				// of those a gap is a fact to report rather than something to fix. Deliberately keyed on
				// NoRestore rather than on Output being nil: both say "not painting a terminal", but
				// NoRestore is the one that says a repaint is unwanted, and it is what the server reads.
				if o.Gap && !opts.NoRestore {
					// Dropped rather than left alone. A reconnect with a position still set resumes, and
					// resuming is exactly what cannot recover a hole.
					*resumeFrom = nil
					// Worth a log line for the same reason a reconnect gets one: the client holds the
					// terminal through it, so the user sees a flicker and nothing else, and a session
					// dropping bytes repeatedly looks identical to one that never did.
					opts.Log.Info("output gap detected, repainting from a fresh attach",
						"session", result.Session, "seq", o.Seq, "bytes", len(o.Data))
					// The chunk is deliberately not written. Its bytes are in the snapshot the fresh
					// attach replays, so writing them here would paint them twice, once against the wrong
					// state.
					return outcomeReconnect, nil
				}
				w := io.Writer(tty)
				if opts.Output != nil {
					w = opts.Output
				}
				if _, err := w.Write(o.Data); err != nil {
					return outcomeDone, err
				}
				next := o.Seq + uint64(len(o.Data))
				*resumeFrom = &next
				if opts.OnOutput != nil {
					// After the write, so a caller told it has reached a position can rely on the bytes up
					// to there having been delivered.
					opts.OnOutput(next)
				}
			}

		case data, ok := <-input:
			if !ok {
				// Input ended. When stdin is a terminal that means the window is gone, and
				// leaving is right: an owning client disconnecting without detaching is what
				// ends its session.
				//
				// When stdin is not a terminal it just means the input is exhausted, which
				// happens immediately with `< /dev/null` or a short pipe. Leaving then would
				// end the attachment before any output arrived, so keep displaying output and
				// wait for the session itself to finish.
				if tty.IsTerminal() {
					return outcomeDone, nil
				}
				input = nil // stop selecting on a closed channel
				continue
			}

			buf, detach := gate.feed(data, time.Now())
			if detach {
				// Forward whatever preceded the detach so a trailing keystroke is not
				// lost, then leave.
				if len(buf) > 0 && !opts.ReadOnly {
					_ = stream.Send(&serverv1.AttachRequest{
						Event: &serverv1.AttachRequest_Input{
							Input: &serverv1.Input{Data: buf},
						},
					})
				}
				// Tell the server this was deliberate, and do not wait to hear back: nothing is at
				// risk if the message is discarded by the connection closing behind it.
				_ = stream.Send(&serverv1.AttachRequest{
					Event: &serverv1.AttachRequest_Detach{
						Detach: &serverv1.Detach{NoAck: true},
					},
				})
				result.Detached = true
				return outcomeDone, nil
			}

			// Rearmed from the deadline the gate reports rather than restarted here, so a run of
			// keystrokes each ending in a partial cannot postpone the release indefinitely: the
			// deadline belongs to the first byte withheld.
			stopHold()
			if deadline, holding := gate.deadline(); holding {
				holdTimer = time.NewTimer(time.Until(deadline))
				holdC = holdTimer.C
			}

			if sendInput(buf) {
				// The server went away mid-keystroke. The bytes are held for the reconnect by
				// sendInput itself.
				return outcomeReconnect, nil
			}

		case <-holdC:
			// The grace expired, so what looked like the start of a detach sequence was a keypress in
			// its own right. Almost always a lone escape.
			stopHold()
			if sendInput(gate.flush()) {
				return outcomeReconnect, nil
			}

		case err := <-inputErr:
			// EOF on a non-terminal stdin is exhausted input, not a reason to stop: the
			// session's output is still worth displaying until it ends.
			if errors.Is(err, io.EOF) && !tty.IsTerminal() {
				inputErr = nil
				continue
			}
			// Reading the terminal failed, which reconnecting cannot fix.
			if err != nil && !errors.Is(err, io.EOF) {
				return outcomeDone, err
			}
			return outcomeDone, nil

		case <-winch:
			if opts.ReadOnly {
				continue
			}
			rows, cols := tty.Size()
			if rows == 0 || cols == 0 {
				continue
			}
			// Pixels are re-read on every resize rather than remembered from the attach: the window
			// changing size is exactly when they change, and a font size change moves them without
			// changing the cell count at all.
			xpixel, ypixel := tty.PixelSize()
			if err := stream.Send(&serverv1.AttachRequest{
				Event: &serverv1.AttachRequest_Resize{
					Resize: &serverv1.Resize{
						Rows: uint32(rows), Cols: uint32(cols),
						XPixel: uint32(xpixel), YPixel: uint32(ypixel),
					},
				},
			}); err != nil {
				return outcomeReconnect, nil
			}

		case <-ctx.Done():
			// Interrupted, such as by SIGTERM. Detach rather than abandoning the stream, so the exit is
			// recorded as deliberate in the server's log rather than looking like a client that died.
			_ = stream.Send(&serverv1.AttachRequest{
				Event: &serverv1.AttachRequest_Detach{
					Detach: &serverv1.Detach{NoAck: true},
				},
			})
			result.Detached = true
			return outcomeDone, nil
		}
	}
}

// readInput forwards terminal input until it fails.
//
// A read blocked in the kernel cannot be cancelled, so this goroutine is intentionally
// allowed to outlive a connection and is reused across reconnects.
func readInput(tty *TTY, out chan<- []byte, errc chan<- error) {
	defer close(out)
	buf := make([]byte, inputReadSize)
	for {
		n, err := tty.Read(buf)
		if n > 0 {
			// Copy: the buffer is reused on the next read.
			data := make([]byte, n)
			copy(data, buf[:n])
			out <- data
		}
		if err != nil {
			errc <- err
			return
		}
	}
}

// dial connects to the server's socket.
func dial(socketPath string) (transport.Conn, serverv1.ServerClient, error) {
	return transport.DialServer(socketPath)
}

// outMsg is one message from the server, or the error that ended the stream.
type outMsg struct {
	resp *serverv1.AttachResponse
	err  error
}

// discardLogHandler drops every record, for a client that was given no logger.
//
// Defaulted rather than nil-checking at each use: a logger is called from the reconnect path and from
// error handling, where an extra branch per call would obscure the logic being read.
type discardLogHandler struct{}

func (discardLogHandler) Enabled(context.Context, slog.Level) bool  { return false }
func (discardLogHandler) Handle(context.Context, slog.Record) error { return nil }
func (h discardLogHandler) WithAttrs([]slog.Attr) slog.Handler      { return h }
func (h discardLogHandler) WithGroup(string) slog.Handler           { return h }

// resumeFromValue renders a resume point for a log line.
//
// A nil pointer means "wherever the server decides", which is a meaningful state rather than a missing value,
// so it is logged as -1 rather than as an empty field.
func resumeFromValue(p *uint64) int64 {
	if p == nil {
		return -1
	}
	return int64(*p)
}
