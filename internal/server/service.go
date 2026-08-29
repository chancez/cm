package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/chancez/cm/internal/fault"
	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/tags"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// enterDelay is how long a send waits between writing its text and writing the CR that submits it.
//
// Needed because a full-screen reader doing paste detection is still assembling the burst when the CR
// arrives, and consumes it as pasted content rather than as the submitting key. Splitting the writes
// without a gap fixed 845 bytes but not 1643; 50ms was enough at 2843 bytes across repeated runs, and
// 200ms held at every size tried. Set above the measured floor rather than at it, since the reader's
// timing is not cm's to control and the cost is paid once per send.
//
// See writeInputThenEnter for the measurements and the reported symptom.
const enterDelay = 200 * time.Millisecond

// Service adapts a Manager to the client-facing ttrpc API.
type Service struct {
	mgr *Manager
	// stop ends the server. Set by Serve, so a Shutdown RPC and a signal take the same path.
	stop atomic.Pointer[context.CancelFunc]
}

// setStop gives the service a way to end the server it is running under.
func (s *Service) setStop(stop context.CancelFunc) {
	s.stop.Store(&stop)
}

// Shutdown stops the server, leaving every session running.
//
// Sessions survive because each shim owns its pty, which is what makes a server upgrade
// survivable for a shell. The next server adopts them through Reconcile.
//
// Returns before the server has finished stopping. It cannot do otherwise: replying travels over the
// connection that shutdown closes, so waiting would mean the caller never hears back.
func (s *Service) Shutdown(
	_ context.Context, _ *serverv1.ShutdownRequest,
) (*serverv1.ShutdownResponse, error) {
	stop := s.stop.Load()
	if stop == nil {
		return nil, errors.New("this server cannot be shut down remotely")
	}
	s.mgr.log.Info("shutting down on request, leaving sessions running")
	// Asynchronously, so this reply is sent before the connection carrying it is torn down.
	go (*stop)()
	return &serverv1.ShutdownResponse{}, nil
}

// NewService wraps a manager.
func NewService(m *Manager) *Service { return &Service{mgr: m} }

// routeInput sends each framed part to its owner.
//
// A terminal can place a reply beside a keypress, and a reply may span several reads. Keeping the
// routing here makes both cases take the same path: only a complete recognized reply reaches the query
// matcher, and ordinary input still reaches the program in order.
func routeInput(ctx context.Context, sess *Session, tok *attachToken, parts []input.Part) error {
	for _, part := range parts {
		if part.Reply {
			sess.answerFromClient(tok, part.Data)
			continue
		}
		if err := sess.Write(ctx, part.Data); err != nil {
			return fmt.Errorf("writing to session %s: %w", sess.id, err)
		}
	}
	return nil
}

// frameInput releases an expired reply fragment before framing new input, so a missing terminator cannot
// turn a later keypress into part of a terminal response.
func frameInput(framer *input.ReplyFramer, data []byte, now time.Time) []input.Part {
	parts := framer.FlushExpired(now)
	return append(parts, framer.Split(data, now)...)
}

// openOptionsFrom translates a client's Open into the options a session is created from.
//
// A function rather than a literal inside Attach so what it copies can be asserted without a stream, a
// terminal, or a spawned shim. The failure mode is silent and has happened: a field present on the wire
// and missing here produces a session that works with one property quietly absent, which is how a
// client's pixel size reached the server and never reached the pty. `kitten icat` then refused to draw
// and blamed the terminal, so nothing pointed at cm.
//
// Note ResumeFromSeq, ReadOnly, and NoRestore are deliberately absent: they describe this attachment
// rather than the session, and Attach reads them directly.
func openOptionsFrom(open *serverv1.Open) OpenOptions {
	return OpenOptions{
		Ref:           open.Session,
		Rows:          uint16(open.Rows),
		Cols:          uint16(open.Cols),
		XPixel:        uint16(open.XPixel),
		YPixel:        uint16(open.YPixel),
		Command:       open.Command,
		Dir:           open.Cwd,
		Env:           open.Env,
		ClientEnv:     open.ClientEnv,
		Persist:       open.Persist,
		CaptureOutput: open.CaptureOutput,
		OnRestore:     RestoreAction(open.OnRestore),
		Tags:          open.Tags,
		// Carried so a reader is not given a new shell in place of the session it asked to observe.
		ReadOnly: open.ReadOnly,
	}
}

// Attach runs one client's attachment for its lifetime.
//
// The first message must be an Open. After that, input, resize, and detach arrive on the
// same stream so their order relative to keystrokes is preserved: a resize that overtook
// pending input would repaint at the wrong size.
func (s *Service) Attach(ctx context.Context, srv serverv1.Server_AttachServer) error {
	first, err := srv.Recv()
	if err != nil {
		return err
	}
	open := first.GetOpen()
	if open == nil {
		return errors.New("first message on an attach stream must be Open")
	}
	// Validated here as well as in the CLI, because this is the trust boundary: the CLI is one
	// client of many, and a tag reaching the store unchecked would be printed to the terminal of
	// whoever runs `cm list`. That is the whole reason the character set excludes escape sequences.
	if err := tags.Validate(open.Tags); err != nil {
		return err
	}

	sess, created, err := s.mgr.Open(ctx, openOptionsFrom(open))
	if err != nil {
		// A reader asked for a session whose shell is gone, so it is told that rather than given a new
		// one. Answered with the same Opened-then-Exited pair as an exit observed mid-stream, since a
		// follower cannot tell which side of its attach the exit fell on and must not have to.
		var ended *EndedSessionError
		if errors.As(err, &ended) {
			return s.sendEndedSession(srv, ended)
		}
		return err
	}

	// Record what this client's terminal looks like, on every attach rather than only on
	// creation. A shell that has been running for days holds values describing a terminal that
	// may have been replaced since, and this is the only record of the current one.
	//
	// After Open rather than before, because a session created by this call has no row to update
	// until then.
	if len(open.ClientEnv) > 0 {
		if err := s.mgr.store.Apply(ctx, sess.id, store.Update{Env: open.ClientEnv}); err != nil {
			// Advisory: the session works, but `get-env` will hand out stale values, which is
			// otherwise mysterious.
			s.mgr.log.Warn("recording client environment failed",
				"session", sess.id, "error", err)
		}
	}

	// Resize before snapshotting, not after.
	//
	// The order matters and is easy to get backwards. A snapshot taken at the session's old size
	// describes lines wrapped for that width; a client of a different width then wraps them
	// again, and the screen arrives visibly mangled. Resizing first means the model reflows once,
	// and the snapshot describes what this client will actually display.
	//
	// This is enforced by reserving the client's sizing slot here, before attaching, because the
	// policy needs a token to decide with and taking it from attach is what broke the order once
	// already. See TestAttachResizesBeforeSnapshotting.
	//
	// A client that wants the screen repainted is displaying the session, which is what distinguishes it
	// from one attaching only to create the session or to stream bytes. Recorded here rather than derived
	// later, since the request is the only place the distinction exists.
	if !open.NoRestore {
		sess.noteWatched()
	}

	tok := sess.reserveClient()
	// Recorded as soon as there is a token to hang it on, so a client that fails between here and
	// attaching is still described while it exists. Advisory and never used for a decision: an older
	// client sends neither field, and the zero values report as unknown.
	sess.noteClientIdentity(tok, open.ClientVersion, open.ClientPid)
	if open.ReadOnly {
		// Recorded before anything is pumped. A follower cannot answer a terminal query, since its
		// input is dropped, and counting one as an answerer makes the emulator stay silent and the
		// querying program hang.
		sess.markReadOnly(tok)
	}

	// Sized before the screen is taken, so the snapshot below describes this client's width. Whether
	// this client's size wins depends on the configured policy, which only matters once several
	// clients are attached. On resume the client already matches, so nothing is resized and the shell
	// is not made to redraw for no reason.
	if open.ResumeFromSeq == nil {
		if err := s.sizeForAttach(ctx, sess, tok, open); err != nil {
			sess.releaseClient(tok)
			return err
		}
	}

	// The wire carries a plain uint64. A resume position is in the log's numbering, since it is what
	// this client last received, so that is what it becomes here.
	var resumeFrom *seq.Log
	if open.ResumeFromSeq != nil {
		resumeFrom = new(seq.Log(*open.ResumeFromSeq))
	}
	att, err := sess.attach(resumeFrom, tok)
	if err != nil {
		sess.releaseClient(tok)
		if errors.Is(err, ErrSessionGone) {
			// The shell exited between Open and here. Report it the way the streaming loop below
			// does rather than failing the call: the session really did run and really did exit, so
			// "session has ended" as an *error* loses the exit code and turns a successful short
			// command into an RPC failure.
			//
			// This is a race a fast command loses often enough to matter. `cm run -- false` exited
			// 1 with "session has ended" instead of the command's status roughly one time in
			// twenty-five, and more often on a loaded machine.
			//
			// Opened is sent first so this looks like every other attach: a client that has just
			// been told the session name and then told it exited needs no special case, whereas a
			// stream that opens with Exited would break the invariant that Opened comes first.
			if err := srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Opened{
					Opened: &serverv1.Opened{
						Session:   sess.Label(),
						SessionId: sess.id,
						Created:   created,
						NextSeq:   uint64(sess.LastSeq()),
					},
				},
			}); err != nil {
				return err
			}
			return s.sendExited(srv, sess)
		}
		return err
	}

	// A client attaching from inside another session freezes that session's metadata for as long as
	// it runs. See Session.beginHosting: the parent's shell is blocked inside `cm attach`, so every
	// report on its pty for this interval is really this child's, arriving there only because the
	// child's output passes through it.
	//
	// Registered after attach succeeded and paired with a defer, so a session cannot be left frozen
	// by an attach that failed on one of the paths above. A parent stuck believing it is hosting
	// forever would stop reporting its own directory for the rest of its life, which is a worse bug
	// than the one being fixed.
	//
	// The parent may not be a session this server knows: it can have exited, or the client can name
	// one from a different server, and neither is an error worth failing an attach over. Nothing is
	// frozen in that case, which is correct, because there is no parent whose bookkeeping could be
	// wrong.
	if open.InsideSession != "" && open.InsideSession != sess.id {
		if parent, live := s.mgr.Get(open.InsideSession); live {
			parent.beginHosting(sess.id)
			defer parent.endHosting(sess.id)
		}
	}

	s.mgr.log.Info("client attached",
		"session", sess.id, "created", created, "resuming", open.ResumeFromSeq != nil,
		"read_only", open.ReadOnly,
		"inside", open.InsideSession, "restore_bytes", len(att.restore))
	reader := att.reader
	defer func() {
		// Tell a program that tracks focus when the last client leaves, since a detached session
		// is exactly "nobody is watching".
		if last := sess.detach(att); last {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			sess.ReportFocus(ctx, false)
		}
	}()
	startSeq := reader.Position()

	// And when one arrives.
	if att.first {
		sess.ReportFocus(ctx, true)
	}

	// The screen repaint, unless the client asked to go without it.
	//
	// A client painting a terminal needs it: an empty window has to be filled with the session's current
	// screen. A follower piping output to a file wants the opposite, since the repaint duplicates whatever it
	// has already printed -- which is what `cm read --follow` did before this option existed, printing its
	// last lines twice, once rendered and once inside the restored screen.
	restore := att.restore
	if open.NoRestore {
		restore = nil
	}
	if err := srv.Send(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Opened{
			Opened: &serverv1.Opened{
				Session:   sess.Label(),
				SessionId: sess.id,
				Created:   created,
				Restore:   restore,
				NextSeq:   uint64(startSeq),
			},
		},
	}); err != nil {
		return err
	}

	// The client has been told the session exists and has received none of it. A test that has to act on
	// the session while a client sits in that gap pauses here; the follower-revive bug was found in this
	// window by accident.
	fault.At(fault.AfterAttachOpened)

	// A detach and a dropped connection are the same outcome here: this client stops watching and
	// the session keeps running. They were once distinguished, because an owned session ended when
	// its client vanished without detaching, and telling the two apart needed a flag set on arrival
	// rather than inferred from how the stream closed. Ownership is gone, so nothing consults the
	// difference.
	detached := make(chan struct{})
	recvErr := make(chan error, 1)
	go func() {
		recvErr <- s.recvLoop(ctx, sess, srv, open.ReadOnly, att.token, detached)
	}()

	// Metadata is forwarded so a terminal emulator can retitle a tab or open a new window in the
	// session's directory. Subscribing before the loop means the current values arrive
	// immediately rather than only on the next change.
	metaSub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(metaSub)

	// Nil for a client with no entry, and a nil channel blocks forever, which keeps this case out of the
	// select for anything that cannot be repainted.
	repaint := sess.repaintChan(att.token)

	// Output is read on its own goroutine because the reader blocks, and this loop also has
	// to notice a detach or a dropped connection.
	type chunkMsg struct {
		chunk seqlog.Chunk[seq.Log]
		err   error
	}
	chunks := make(chan chunkMsg, 1)
	go func() {
		defer close(chunks)
		for {
			c, err := reader.Next(ctx)
			select {
			case chunks <- chunkMsg{c, err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	for {
		select {
		case msg, ok := <-chunks:
			if !ok {
				return nil
			}
			if msg.err != nil {
				// The log closed, meaning the session ended. Report the exit status so the
				// client stops rather than trying to reconnect to a session that is gone.
				if errors.Is(msg.err, seqlog.ErrClosed) {
					if ended, _ := sess.Ended(); ended {
						return s.sendExited(srv, sess)
					}
					return nil
				}
				return msg.err
			}
			if err := srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Output{
					Output: &serverv1.Output{
						Seq:  uint64(msg.chunk.Seq),
						Data: msg.chunk.Data,
						Gap:  msg.chunk.Gap,
					},
				},
			}); err != nil {
				return err
			}

		case <-repaint:
			// The session left the alternate screen and this client attached during the program, so its
			// terminal's main screen holds whatever its own window held before. An empty chunk flagged as a
			// gap is the signal: the client drops its resume position and reattaches, and a fresh attach
			// answers with a serialized screen describing the main screen it never had.
			//
			// Empty rather than carrying bytes, because there are none to carry. The client's gap branch
			// deliberately does not write the chunk's data, so nothing is lost by it being nil, and the
			// position is this client's current one for the log line rather than for a resume it is about
			// to discard.
			//
			// Sent from this loop rather than pushed from the session, for the same reason a query is: this
			// is the only goroutine that may write to the stream.
			if err := srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Output{
					Output: &serverv1.Output{Seq: uint64(sess.ClientSeq()), Gap: true},
				},
			}); err != nil {
				return err
			}

		case q := <-att.queries:
			// A question cm cannot answer itself, addressed to this client's terminal. The client writes
			// it out and the reply comes back on the input path, where answerFromClient matches it to the
			// request. Read-only followers are never sent one, since their input is dropped.
			if err := srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Query{
					Query: &serverv1.Query{Data: q},
				},
			}); err != nil {
				return err
			}

		case meta := <-metaSub.ch:
			if err := srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Metadata{
					Metadata: &serverv1.Metadata{
						Title:      meta.Title,
						Cwd:        meta.Cwd.Path,
						CwdIsLocal: meta.Cwd.IsLocal,
						Busy: meta.Command.Running || meta.Reported.State == "busy" ||
							meta.Reported.State == "blocked",
						Command:             meta.Command.Command,
						LastCommandExitCode: int32(meta.Command.ExitCode),
						CommandFinished:     meta.Command.Exited,
						ReportedState:       meta.Reported.State,
						ReportedDetail:      meta.Reported.Detail,
					},
				},
			}); err != nil {
				return err
			}

		case <-detached:
			// Deliberate detach: the session keeps running.
			return nil

		case <-att.evict:
			// `cm detach` asked this client to let go, or `cm clients upgrade` asked it to come back on a
			// newer build.
			//
			// Told before the stream closes, so the client can report "detached by request" rather than
			// treating a server-initiated close as an outage and trying to reconnect. Failure is ignored:
			// the client is going away regardless, and this is the same best-effort notice the recv loop
			// sends.
			//
			// The upgrade flag is read here rather than carried on the channel, which is a bare signal.
			// It is set before the close for that reason, so by the time this wakes it is already there.
			upgrade := sess.isUpgrading(att.token)
			switchTo := sess.switchTarget(att.token)
			_ = srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Detached{
					Detached: &serverv1.Detached{Upgrade: upgrade, SwitchTo: switchTo},
				},
			})
			switch {
			case switchTo != "":
				s.mgr.log.Info("client asked to switch",
					"session", sess.id, "switch_to", switchTo)
			case upgrade:
				s.mgr.log.Info("client asked to upgrade", "session", sess.id)
			default:
				s.mgr.log.Info("client detached on request", "session", sess.id)
			}
			return nil

		case err := <-recvErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil

		case <-ctx.Done():
			// The request context is cancelled when the connection drops, which races with the receive
			// loop failing. Either one ending this attachment is correct: the session outlives both.
			return ctx.Err()
		}
	}
}

// sizeForAttach settles the session's size for a freshly attaching client, before its screen is
// serialized.
//
// Called with a reserved token rather than an attachment, which is what lets it run ahead of the
// snapshot. Returns an error only when the attach itself should fail; a shim that has gone away is
// reported to the log and left to the streaming loop, which has the exit status.
func (s *Service) sizeForAttach(
	ctx context.Context,
	sess *Session,
	tok *attachToken,
	open *serverv1.Open,
) error {
	rows, cols, x, y, resize := sess.registerClientSize(
		tok, uint16(open.Rows), uint16(open.Cols),
		uint16(open.XPixel), uint16(open.YPixel), open.ReadOnly,
	)
	if !resize {
		return nil
	}

	// ResizeSignal rather than Resize: on a fresh attach the shell has to redraw even when the size
	// is unchanged, or a program that repaints only on SIGWINCH keeps updating a screen that is now
	// the snapshot replayed below.
	//
	// The redraw this provokes is also why the ordering here matters beyond wrapping. Whatever the
	// shell emits in response is generated now, ahead of the snapshot, so it is part of the screen
	// being serialized instead of arriving interleaved with the replay of it.
	if err := sess.ResizeSignal(ctx, uint32(rows), uint32(cols), uint32(x), uint32(y)); err != nil {
		// A shim that has gone away is not a sizing failure, it is the session ending: the attach
		// succeeded, then the shell exited before this resize reached the shim. Sizing a session that
		// no longer exists is moot, so the attach continues and the streaming loop reports the exit
		// status.
		//
		// Without this, `cm run` on a fast command failed with "sizing session s9: ttrpc: closed"
		// instead of the command's exit code, about one run in ten on Linux.
		//
		// Two ways to recognize it, because they can arrive in either order. The session may already
		// be marked ended, or the shim may say so first: the server learns of an exit by watching the
		// output stream, so a resize can reach a shim whose pty is already released while this session
		// still looks alive.
		if ended, _ := sess.Ended(); !ended && !isSessionOver(err) {
			return fmt.Errorf("sizing session %s: %w", sess.id, err)
		}
		s.mgr.log.Info("skipped sizing a session that ended while attaching", "session", sess.id)
	}

	ir, ic := int(rows), int(cols)
	if err := s.mgr.store.Apply(ctx, sess.id, store.Update{Rows: &ir, Cols: &ic}); err != nil {
		s.mgr.log.Warn("recording session size failed", "session", sess.id, "error", err)
	}
	return nil
}

// recvLoop handles client-to-server events until the stream ends.
func (s *Service) recvLoop(
	ctx context.Context,
	sess *Session,
	srv serverv1.Server_AttachServer,
	readOnly bool,
	tok *attachToken,
	detached chan<- struct{},
) error {
	var replies input.ReplyFramer
	type received struct {
		req *serverv1.AttachRequest
		err error
	}
	requests := make(chan received, 1)
	go func() {
		defer close(requests)
		for {
			req, err := srv.Recv()
			select {
			case requests <- received{req: req, err: err}:
			case <-ctx.Done():
				return
			}
			if err != nil {
				return
			}
		}
	}()

	var (
		replyTimer *time.Timer
		replyC     <-chan time.Time
	)
	stopReplyTimer := func() {
		if replyTimer != nil {
			replyTimer.Stop()
			replyTimer = nil
			replyC = nil
		}
	}
	defer stopReplyTimer()
	armReplyTimer := func() {
		stopReplyTimer()
		if deadline, holding := replies.Deadline(); holding {
			replyTimer = time.NewTimer(time.Until(deadline))
			replyC = replyTimer.C
		}
	}
	for {
		select {
		case received, ok := <-requests:
			if !ok {
				return nil
			}
			if received.err != nil {
				return received.err
			}
			req := received.req
			switch {
			case req.GetDetach() != nil:
				// Acknowledged before signalling, so a client that asks can wait for confirmation rather than
				// racing its own disconnect.
				//
				// Skipped when the client said it will not wait. `cm run -d`, `cm attach --no-attach`, and an
				// interrupted follower all detach as their last act and exit; their connection is closing as
				// the Detach arrives, so the reply lost a race about 40% of the time and produced a warning
				// for behavior that was intended, which also made `cm doctor` report a healthy installation as
				// having a problem.
				if !req.GetDetach().NoAck {
					if err := srv.Send(&serverv1.AttachResponse{
						Event: &serverv1.AttachResponse_Detached{Detached: &serverv1.Detached{}},
					}); err != nil {
						// An interactive client that asked for the acknowledgement and lost its connection
						// before it arrived, unlike the case above where not waiting was the intent.
						s.mgr.log.Warn("acknowledging a detach failed",
							"session", sess.id, "error", err)
					}
				}
				close(detached)
				return nil

			case req.GetInput() != nil:
				// A read-only follower's input is dropped rather than refused, so a stray
				// keystroke does not tear down its stream.
				if readOnly {
					continue
				}
				parts := frameInput(&replies, req.GetInput().Data, time.Now())
				armReplyTimer()
				// Events a program asked for and then stopped wanting are dropped before the typing
				// decision, because everything after this treats them as bytes to deliver: they reach
				// the pty and get typed at whatever is reading it now.
				//
				// Reported as "execute: 3u[O_" left at a zsh prompt after quitting codex. codex sets
				// kitty keyboard flags 7 and mode 1004, reads the ctrl-d *press* and exits, and the
				// release arrives after it is gone. See input.IsStaleEvent for why only a release and a
				// focus report qualify, and never a key press.
				//
				// Here rather than inside the not-typed branch below, because IsUserInput already calls
				// a release not-typing: recognizing them was never the problem, nothing acting on it
				// was.
				//
				// After frameInput rather than before, so a fragment the expiry released is filtered on
				// the same terms as anything else. Nothing of that kind can be dropped in practice,
				// since only OSC, DCS and APC are held and this recognizes CSI alone.
				parts = input.DropStaleParts(parts, sess.terminalModes())
				if len(parts) == 0 {
					// Nothing but stale events. Skipped entirely, so this also cannot mark the client
					// as the one being typed in.
					continue
				}
				typed := false
				for _, part := range parts {
					if !part.Reply && input.IsUserInput(part.Data) {
						typed = true
						break
					}
				}
				if !typed {
					// A reply to a terminal query, a mouse report, or a focus change. None of them may claim
					// sizing: otherwise a window nobody is using takes over because the program polled the
					// terminal.
					//
					// A query reply is not written to the pty here, and that is the core of the proxy design.
					// It is handed to the session, which matches it against the question cm asked *this*
					// client and writes it in the order the program's questions were asked. A reply matching
					// no outstanding question is discarded.
					//
					// This replaces electing one client to answer and forwarding its replies straight through.
					// The election could be wrong in four ways and was, each in turn: a read-only follower or a
					// reserved-but-unattached client elected meant nothing answered and the program hung; two
					// attached clients meant a single CSI c came back as "\x1b[?62;52;c\x1b[?62;52;c"; and after
					// a restart cm answered a backlog query that the reconnecting client answered again from
					// the log, typing a git branch name into the prompt. Matching a reply to a request cm
					// actually made removes all four, because an unsolicited reply is now recognizable as such.
					//
					// Mouse and focus events are still forwarded from every client, unchanged. They describe
					// one window rather than the session, so each client sends its own, and dropping them
					// would make a session ignore the mouse in every window but one.
					if err := routeInput(ctx, sess, tok, parts); err != nil {
						return err
					}
					continue
				}
				// Recorded on every keystroke rather than only when sizing moves, so a listing can name
				// the window someone is using under any resize policy. See clientSize.lastInputAt for why
				// typing is the only signal that can identify one client out of several.
				sess.noteClientInput(tok)
				// Typing may transfer sizing, depending on the policy. Checked before the write so
				// the shell is already at the right size when it sees the keystroke.
				if rows, cols, x, y, resize := sess.claimLeadership(tok); resize {
					if err := sess.Resize(ctx, uint32(rows), uint32(cols), uint32(x), uint32(y)); err != nil {
						s.mgr.log.Warn("resizing on leadership change failed",
							"session", sess.id, "error", err)
					} else {
						ir, ic := int(rows), int(cols)
						if err := s.mgr.store.Apply(ctx, sess.id,
							store.Update{Rows: &ir, Cols: &ic}); err != nil {
							s.mgr.log.Warn("recording session size failed",
								"session", sess.id, "error", err)
						}
					}
				}
				if err := routeInput(ctx, sess, tok, parts); err != nil {
					return err
				}

			case req.GetResize() != nil:
				if readOnly {
					continue
				}
				r := req.GetResize()
				// The policy decides whether this client's new size takes effect, so a window being
				// resized while someone else is typing in another does not reflow theirs.
				rows, cols, x, y, resize := sess.registerClientSize(
					tok, uint16(r.Rows), uint16(r.Cols), uint16(r.XPixel), uint16(r.YPixel), readOnly,
				)
				if !resize {
					continue
				}
				if err := sess.Resize(ctx, uint32(rows), uint32(cols), uint32(x), uint32(y)); err != nil {
					return fmt.Errorf("resizing session %s: %w", sess.id, err)
				}
				ir, ic := int(rows), int(cols)
				if err := s.mgr.store.Apply(ctx, sess.id, store.Update{Rows: &ir, Cols: &ic}); err != nil {
					s.mgr.log.Warn("recording session size failed", "session", sess.id, "error", err)
				}
			}

		case <-replyC:
			replyTimer = nil
			replyC = nil
			if err := routeInput(ctx, sess, tok, replies.FlushExpired(time.Now())); err != nil {
				return err
			}

		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

func (s *Service) List(ctx context.Context, req *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	// Parsed before the query so a malformed selector is an error rather than a silent match of
	// everything, which would be the wrong answer for `cm kill --tag`.
	selector, err := tags.ParseSelector(req.Tags)
	if err != nil {
		return nil, err
	}

	records, err := s.mgr.List(ctx)
	if err != nil {
		return nil, err
	}
	// One query for every session's names rather than one per row, since this is the most frequently
	// run command there is.
	namesByID, err := s.mgr.NamesByID(ctx)
	if err != nil {
		return nil, err
	}

	out := &serverv1.ListResponse{Sessions: make([]*serverv1.Session, 0, len(records))}
	for _, rec := range records {
		// Filtered in Go rather than SQL because the tags live in a JSON column, and at these
		// session counts a scan is cheaper than teaching sqlite to index inside it. See the
		// migration for why the column is shaped that way.
		if !selector.Match(rec.Tags) {
			continue
		}
		names := namesByID[rec.ID]
		// A prefix now asks "is this session named something starting with this", which a session can
		// answer several times over or not at all. Matched in Go rather than in SQL: names live in
		// their own table and session counts are in the tens, and doing it here deleted the LIKE
		// escaping the query used to need, where a prefix containing % matched everything.
		if req.Prefix != "" && !anyHasPrefix(names, req.Prefix) {
			continue
		}
		item := &serverv1.Session{
			Name:          Label(rec.ID, names),
			Id:            rec.ID,
			Names:         names,
			ShellPid:      int32(rec.ShellPID),
			Clients:       uint32(s.mgr.Clients(rec.ID)),
			Cwd:           rec.Cwd,
			CwdIsLocal:    true,
			Title:         rec.Title,
			CreatedAtUnix: rec.CreatedAt.Unix(),
			Exited:        rec.State != store.StateRunning,
			ExitCode:      int32(rec.ExitCode),
			State:         sessionState(rec.State),
			// From the record rather than the live session: tags are metadata the store owns, so
			// unlike cwd or the reported state there is no fresher copy in the registry.
			Tags: rec.Tags,
		}
		// A live session's own values are fresher than the stored ones, which lag by a write.
		if sess, live := s.mgr.Get(rec.ID); live {
			// Including its outcome. A session records that it ended before the manager writes it
			// back, so a caller polling for a short-lived command would otherwise see it as running
			// and then briefly as dead, since an unwritten record still says running while the shim
			// is already gone.
			if ended, code := sess.Ended(); ended {
				item.Exited = true
				item.ExitCode = int32(code)
				item.State = serverv1.SessionState_SESSION_STATE_EXITED
				if code < 0 {
					// A negative code is the marker for an unreachable shim rather than a status.
					item.ExitCode = 0
					item.State = serverv1.SessionState_SESSION_STATE_DEAD
				}
			}

			// Only from a live session, like busy below and for the same reason: this describes two
			// live attachments, so a stored value would come back after a restart claiming a nesting
			// that ended with the previous server.
			item.Hosting = sess.Hosting()

			// Same argument as Hosting: an attachment is live state and nothing about it is worth
			// persisting. Kept alongside the count rather than replacing it, since a count is what a
			// status line wants and this is what a diagnosis wants.
			for _, c := range sess.AttachedClients() {
				ac := &serverv1.AttachedClient{
					Pid:      c.PID,
					Version:  c.Version,
					ReadOnly: c.ReadOnly,
					Active:   c.Active,
				}
				// Left at zero rather than sending a bogus timestamp when unknown, which a
				// zero time.Time would become through Unix().
				if !c.AttachedAt.IsZero() {
					ac.AttachedAtUnix = c.AttachedAt.Unix()
				}
				// Same reason, and reached more often: a client that has never typed has no last-input
				// time at all, which is the ordinary state of a window that was just opened.
				if !c.LastInputAt.IsZero() {
					ac.LastInputAtUnix = c.LastInputAt.Unix()
				}
				item.AttachedClients = append(item.AttachedClients, ac)
			}

			title, cwd := sess.Metadata()
			if title != "" {
				item.Title = title
			}
			if cwd.Path != "" {
				item.Cwd = cwd.Path
				item.CwdIsLocal = cwd.IsLocal
			}
			item.CwdUri = sess.CwdURI()

			// Only from a live session, and deliberately not persisted: "a command is running right
			// now" is true of a process, not of a record. A stored value would come back after a
			// restart describing a command that has long since finished.
			cmd := sess.Command()
			item.Busy = cmd.Running
			item.Command = cmd.Command
			// The outcome of the last command, distinct from the session's own status above: that says
			// whether the shell has gone, this says whether the last thing it ran succeeded. Also not
			// persisted, for the same reason -- a stored value would describe a command that finished
			// before the last restart.
			item.LastCommandExitCode = int32(cmd.ExitCode)
			item.CommandFinished = cmd.Exited

			// A program's own report about itself, which takes precedence over the derived state above.
			if r := sess.Reported(); r.State != "" {
				item.ReportedState = r.State
				item.ReportedDetail = r.Detail
				item.ReportedSource = r.Source
				// Reflected in busy too, so a caller reading one field still gets the better answer.
				item.Busy = r.State != "idle"
			}
		}
		out.Sessions = append(out.Sessions, item)
	}
	return out, nil
}

// trimNamePrefix removes a leading quoted session name from a message, since the caller already
// knows which session it applies to.
func trimNamePrefix(msg, name string) string {
	for _, prefix := range []string{
		fmt.Sprintf("%q: ", name),
		name + ": ",
	} {
		if after, ok := strings.CutPrefix(msg, prefix); ok {
			return after
		}
	}
	return msg
}

// sessionState maps a stored state onto the wire enum.
func sessionState(st store.State) serverv1.SessionState {
	switch st {
	case store.StateRunning:
		return serverv1.SessionState_SESSION_STATE_RUNNING
	case store.StateExited:
		return serverv1.SessionState_SESSION_STATE_EXITED
	case store.StateDead:
		return serverv1.SessionState_SESSION_STATE_DEAD
	default:
		return serverv1.SessionState_SESSION_STATE_UNSPECIFIED
	}
}

func (s *Service) Kill(ctx context.Context, req *serverv1.KillRequest) (*serverv1.KillResponse, error) {
	resp := &serverv1.KillResponse{Errors: make(map[string]string)}
	for _, ref := range req.Sessions {
		name := ref
		outcome, err := s.killRef(ctx, ref, req.Force, req.Signal)
		if err == nil && outcome.unboundFrom != "" {
			// The name was a borrower, so it let go and the session runs on. Reported apart from
			// killed, since a caller has to be able to tell that from its session being gone.
			if resp.Unbound == nil {
				resp.Unbound = make(map[string]string)
			}
			resp.Unbound[name] = outcome.unboundFrom
			continue
		}
		surviving := outcome.surviving
		if err != nil {
			// The map is already keyed by name, so a message that repeats it reads as
			// `nosuch: "nosuch": session not found`. Strip the redundant prefix.
			resp.Errors[name] = trimNamePrefix(err.Error(), name)
			continue
		}
		// Killed even when something survived: the session is gone from cm's view and its record is
		// deleted, so this is a leak to warn about rather than a request that failed.
		resp.Killed = append(resp.Killed, name)
		if len(surviving) > 0 {
			if resp.Surviving == nil {
				resp.Surviving = make(map[string]*serverv1.SurvivingProcesses)
			}
			resp.Surviving[name] = &serverv1.SurvivingProcesses{Pids: surviving}
		}
	}
	if len(resp.Errors) == 0 {
		resp.Errors = nil
	}
	return resp, nil
}

// Detach disconnects a session's clients, leaving the session running.
//
// Resolved against live sessions only, and a name the store knows but the registry does not is not an
// error: a session with no server-side presence has no clients to detach, so the request is already
// satisfied. Reporting zero rather than failing is what lets a caller detach a set of sessions without
// first checking which of them anyone is watching.
func (s *Service) Detach(
	_ context.Context, req *serverv1.DetachRequest,
) (*serverv1.DetachResponse, error) {
	resp := &serverv1.DetachResponse{
		Detached: make(map[string]uint32),
		Errors:   make(map[string]string),
	}
	for _, name := range req.Sessions {
		id, err := s.mgr.Resolve(context.Background(), name)
		if err != nil {
			resp.Errors[name] = trimNamePrefix(err.Error(), name)
			continue
		}
		sess, live := s.mgr.Get(id)
		if !live {
			// Distinguished from a name that does not exist at all, which is worth an error: asking to
			// detach something never created is a mistake, while asking to detach an idle session is not.
			if _, err := s.mgr.store.Get(context.Background(), id); err != nil {
				resp.Errors[name] = trimNamePrefix(err.Error(), name)
				continue
			}
			resp.Detached[name] = 0
			continue
		}
		resp.Detached[name] = uint32(sess.EvictClients())
	}
	if len(resp.Errors) == 0 {
		resp.Errors = nil
	}
	return resp, nil
}

// UpgradeClients asks attached clients to replace themselves with the newest binary on disk.
//
// An empty session list means every session, unlike Detach, which requires names. The asymmetry is
// deliberate and follows what each command is for: detaching one window while leaving others is an
// ordinary thing to want, while upgrading one window and leaving the rest on an old build is the state
// this command exists to get out of.
//
// Resolved against live sessions only, on the same reasoning as Detach: a session nothing is attached to
// has no client to upgrade, so zero is a satisfied request rather than a failure.
func (s *Service) UpgradeClients(
	ctx context.Context, req *serverv1.UpgradeClientsRequest,
) (*serverv1.UpgradeClientsResponse, error) {
	names := req.Sessions
	if len(names) == 0 {
		records, err := s.mgr.List(ctx)
		if err != nil {
			return nil, fmt.Errorf("listing sessions to upgrade: %w", err)
		}
		for _, rec := range records {
			names = append(names, paths.FormatSessionID(rec.ID))
		}
	}

	resp := &serverv1.UpgradeClientsResponse{
		Asked:          make(map[string]uint32),
		AlreadyCurrent: make(map[string]uint32),
		Errors:         make(map[string]string),
	}
	// The build clients are asked to converge on. The server's own, since it is the newest thing known to
	// be installed: a client re-execs the binary on disk, and the server was started from that same path.
	current := s.mgr.Version()
	for _, name := range names {
		id, err := s.mgr.Resolve(ctx, name)
		if err != nil {
			resp.Errors[name] = trimNamePrefix(err.Error(), name)
			continue
		}
		sess, live := s.mgr.Get(id)
		if !live {
			if _, err := s.mgr.store.Get(ctx, id); err != nil {
				resp.Errors[name] = trimNamePrefix(err.Error(), name)
				continue
			}
			resp.Asked[name] = 0
			continue
		}
		asked, skipped := sess.UpgradeClients(req.Force, current, req.ActiveOnly)
		resp.Asked[name] = uint32(asked)
		if skipped > 0 {
			resp.AlreadyCurrent[name] = uint32(skipped)
		}
	}
	// Omitted rather than sent empty, matching Detach, so a caller checking for problems tests one thing.
	if len(resp.Errors) == 0 {
		resp.Errors = nil
	}
	if len(resp.AlreadyCurrent) == 0 {
		resp.AlreadyCurrent = nil
	}
	return resp, nil
}

func (s *Service) Send(ctx context.Context, req *serverv1.SendRequest) (*serverv1.SendResponse, error) {
	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	sess, ok := s.mgr.Get(id)
	if !ok {
		return nil, fmt.Errorf("%q: %w", req.Session, store.ErrNotFound)
	}

	if req.Match != "" && req.WaitUntil != serverv1.WaitState_WAIT_STATE_UNSPECIFIED {
		// Refused rather than combined, matching Wait: "idle and also matching" and "idle or matching" are
		// both plausible readings of the pair.
		return nil, errors.New("match and a state cannot both be waited for")
	}
	if req.MatchRaw && req.Match == "" {
		return nil, errors.New("match_raw only applies with match")
	}

	if req.Match != "" {
		return s.sendAndAwaitMatch(ctx, sess, req)
	}

	if req.WaitUntil == serverv1.WaitState_WAIT_STATE_UNSPECIFIED {
		if err := writeInputThenEnter(ctx, sess, req.Data, req.Enter); err != nil {
			return nil, err
		}
		// StateRuns rather than CommandRuns, so a session driven by explicit reports is not described as
		// reporting nothing. The flag exists to warn that a wait may never resolve, and a reporting session
		// resolves fine.
		return &serverv1.SendResponse{ShellReports: sess.StateRuns() > 0}, nil
	}

	// Subscribe before writing, so the wait cannot miss what the input causes.
	//
	// This ordering is the entire reason a combined send-and-wait exists. Two separate calls cannot be
	// ordered from outside: the command runs as soon as the input lands, so a fast one finishes before a
	// following Wait arrives, and that wait then blocks until its timeout having missed the transition
	// it was created for. Arming first closes the window rather than narrowing it.
	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)
	// Drain the seeded value, which describes the session as it was before the input.
	select {
	case <-sub.ch:
	default:
	}

	// Recorded before the input, so a command that starts and finishes too fast to observe is still
	// evident from the count having moved.
	runsBefore := sess.StateRuns()

	if err := writeInputThenEnter(ctx, sess, req.Data, req.Enter); err != nil {
		return nil, err
	}

	// afterInput, so a wait for idle means "the command I just sent finished" rather than "the shell is
	// at a prompt", which it already was.
	wait, err := s.awaitState(ctx, sess, sub, req.WaitUntil, req.WaitTimeoutMs, true, runsBefore)
	if err != nil {
		return nil, err
	}
	// runsBefore, taken before the input, rather than the count now: a caller warning that a wait may never
	// resolve wants to know whether the shell was already reporting, and this call's own command would
	// otherwise make every session look like it reports.
	return &serverv1.SendResponse{Wait: wait, ShellReports: runsBefore > 0}, nil
}

// writeInputThenEnter writes a send's text and its submitting keypress as two separate pty writes.
//
// Two writes rather than one concatenated buffer, because a pty read returns at most 1022 bytes and one
// large write therefore arrives as several reads: 1201 bytes came back as [1022, 179], measured. A
// full-screen program doing paste detection sees that multi-read burst as a paste and consumes a trailing
// CR as pasted content rather than as the keypress that submits it.
//
// The symptom is a send that lands in the program's input box and sits there. Reported driving a Claude
// Code session, which showed the text as "[Pasted text #4]" unsubmitted, where a second
// `cm send --key enter` submitted it. Measured against a real one with only the length varying: 42 bytes
// submitted, 121 and 281 bytes landed without submitting, 842 bytes did not appear until a separate enter
// arrived, and two writes submitted at every size.
//
// Both writes happen here, inside whatever wait the caller armed, because the command starts on the CR: a
// client making two Send calls would arm its wait after the text and miss the transition.
//
// A short shell prompt is unaffected either way, since it reads a small write in one go, so this is not a
// behavior change for the ordinary case. It is deliberately not done in Session.Write, which also carries
// query replies, where splitting a sequence from its terminator would be the bug rather than the fix.
//
// Splitting alone is not sufficient above about 1 KB, which is the part that took measuring. A bare split
// fixed 845 bytes but not 1643, because the reader was still mid-paste when the CR landed and swallowed it
// anyway. The gap is what makes it work: 2843 bytes submitted reliably with as little as 50ms between the
// writes, and failed with none. That is why enterDelay exists rather than the two writes going out
// back-to-back, and it is also why the original two-call workaround succeeded, since an RPC round trip
// supplied the same gap by accident.
func writeInputThenEnter(ctx context.Context, sess *Session, data, enter []byte) error {
	if len(data) > 0 {
		if err := sess.Write(ctx, data); err != nil {
			return err
		}
	}
	if len(enter) == 0 {
		return nil
	}
	// Only after text, since a keys-only send has no paste for the reader to still be assembling.
	if len(data) > 0 {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(enterDelay):
		}
	}
	return sess.Write(ctx, enter)
}

// readableSession returns the session to render from, or nil to read from disk instead.
//
// Not the same question as "is it in the registry", which is what this replaced and why `cm run` would
// occasionally print nothing at all.
//
// A session's terminal model is discarded the moment its shell exits, but the session is removed from the
// registry by a separate goroutine afterwards. Between those two events it is still found here while having
// nothing left to render, so rendering it returns empty rather than falling back to the log on disk. The window
// is short, which is what made this look like flakiness rather than a bug: `cm run` printed nothing in 4 runs
// out of 40, and re-reading immediately afterwards returned the output, because by then the removal had
// happened. Widening the window by delaying the removal made it fail every time, which is what identified it.
//
// Treating an ended session as not-live closes it, since the on-disk log holds the same bytes and is complete:
// the shim appends unbuffered before it reports the exit, so anything the command printed is already there.
//
// Checked via Ended rather than by looking for a nil terminal, so the decision does not depend on a field
// another goroutine owns, and so a session built without an emulator still takes the same path it always did.
func (s *Service) readableSession(id string) *Session {
	sess, live := s.mgr.Get(id)
	if !live {
		return nil
	}
	if ended, _ := sess.Ended(); ended {
		return nil
	}
	return sess
}

func (s *Service) History(ctx context.Context, req *serverv1.HistoryRequest) (*serverv1.HistoryResponse, error) {
	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	sess := s.readableSession(id)
	if sess == nil {
		// A session that has ended has no terminal model, whether or not it has left the registry yet. If it
		// was persisting, its output is still on disk and can be replayed, which is what makes reading a
		// finished command's output possible at all.
		data, err := s.mgr.HistoryFromDisk(ctx, id, req.Format)
		if err != nil {
			return nil, err
		}
		return &serverv1.HistoryResponse{Data: data}, nil
	}

	var format HistoryFormat
	switch req.Format {
	case serverv1.HistoryFormat_HISTORY_FORMAT_VT:
		format = HistoryVT
	case serverv1.HistoryFormat_HISTORY_FORMAT_HTML:
		format = HistoryHTML
	default:
		format = HistoryPlain
	}

	data, err := sess.History(format)
	if err != nil {
		return nil, err
	}
	return &serverv1.HistoryResponse{Data: data}, nil
}

// Read returns a bounded, parseable view of a session's recent output.
//
// Distinct from History, which renders the whole scrollback: a caller reading a session
// programmatically wants the tail, and wants soft-wrapped lines rejoined so a path the terminal broke to
// fit its width is one line again.
func (s *Service) Read(ctx context.Context, req *serverv1.ReadRequest) (*serverv1.ReadResponse, error) {
	if req.SinceCommands > 0 && req.LastOutput {
		return nil, errors.New("since_commands and last_output cannot both be set")
	}

	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	sess := s.readableSession(id)
	if sess == nil {
		if req.SinceCommands > 0 || req.LastOutput {
			// Command boundaries live in memory alongside the session, so a session that has ended has
			// none. Reported plainly rather than falling back to a line count, since quietly answering a
			// different question than the one asked is how a caller comes to trust output that does not
			// mean what it thinks.
			return nil, fmt.Errorf(
				"session %s has ended, so its command boundaries are gone; read it by lines instead",
				req.Session)
		}
		// A finished session is the common case here, not an edge one: `cm run` waits for its command, so
		// the session has already ended by the time anything reads its output.
		data, err := s.mgr.ReadFromDisk(ctx, id, int(req.Lines), req.Unwrap, req.Raw)
		if err != nil {
			return nil, err
		}
		return &serverv1.ReadResponse{Data: data}, nil
	}

	if req.SinceCommands > 0 || req.LastOutput {
		return s.readFromCommand(sess, req)
	}

	if req.Raw {
		data, err := sess.ReadVT(int(req.Lines))
		if err != nil {
			return nil, err
		}
		return &serverv1.ReadResponse{Data: data}, nil
	}

	data, err := sess.Read(int(req.Lines), req.Unwrap)
	if err != nil {
		return nil, err
	}
	return &serverv1.ReadResponse{Data: data}, nil
}

// readFromCommand serves a read anchored at a command boundary rather than a line count.
//
// Renders from a slice of the session's log rather than from its terminal model, because a model holds
// the current screen: it cannot answer "what did the command before this one print", and the earlier
// output may have scrolled off it entirely while still being in the log.
func (s *Service) readFromCommand(
	sess *Session, req *serverv1.ReadRequest,
) (*serverv1.ReadResponse, error) {
	var (
		from seq.Log
		err  error
	)
	if req.LastOutput {
		from, err = sess.LastOutput()
	} else {
		from, _, err = sess.SinceCommands(int(req.SinceCommands))
	}
	if err != nil {
		if errors.Is(err, ErrNoCommandBoundaries) {
			// The cause is almost always a shell with no OSC 133 integration rather than a session that
			// has run nothing, and the symptom otherwise looks like a command that printed nothing. The
			// same condition `cm doctor`'s no-shell-integration check exists for, so the message points
			// at it.
			return nil, fmt.Errorf(
				"session %s has not reported any commands, so there is no boundary to read from; "+
					"its shell needs OSC 133 integration loaded (see `%s doctor`)",
				req.Session, paths.Name)
		}
		return nil, err
	}

	data, gap := sess.SnapshotFrom(from)
	if gap {
		// Reported rather than silently serving less. A read anchored at a command boundary that begins
		// mid-command is worse than a short one, because the caller believes it has the whole output.
		s.mgr.log.Warn("command output partly aged out of the buffer",
			"session", req.Session, "from_seq", from)
	}

	rows, cols := sess.Size()
	rendered, err := s.mgr.RenderSnapshot(data, rows, cols, req.Unwrap, req.Raw)
	if err != nil {
		return nil, err
	}
	return &serverv1.ReadResponse{Data: rendered}, nil
}

func (s *Service) GetEnv(ctx context.Context, req *serverv1.GetEnvRequest) (*serverv1.GetEnvResponse, error) {
	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	rec, err := s.mgr.store.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return &serverv1.GetEnvResponse{Env: rec.Env}, nil
}

// describeCommand renders an argv for display, quoting only when needed so the common case
// stays readable.
func describeCommand(argv []string) string {
	parts := make([]string, 0, len(argv))
	for _, a := range argv {
		if strings.ContainsAny(a, " \t\n\"'\\") {
			parts = append(parts, fmt.Sprintf("%q", a))
			continue
		}
		parts = append(parts, a)
	}
	return strings.Join(parts, " ")
}

// sendExited reports a session's exit status to a client.
//
// Shared by the two places a client can learn a session ended: the output stream closing, and an
// attach that arrived after the shell had already exited. Both must say the same thing, since the
// client exits with whatever code it is told and a short-lived command can take either path
// depending on timing.
func (s *Service) sendExited(srv serverv1.Server_AttachServer, sess *Session) error {
	_, code := sess.Ended()
	return srv.Send(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Exited{
			Exited: &serverv1.Exited{ExitCode: int32(code)},
		},
	})
}

// sendEndedSession answers a read-only attach to a session that has already finished.
//
// Opened first, then Exited, which is the order every other attach uses: a client that has been told the
// session name and then told it exited needs no special case, whereas a stream opening with Exited would
// break the invariant that Opened comes first.
func (s *Service) sendEndedSession(srv serverv1.Server_AttachServer, ended *EndedSessionError) error {
	if err := srv.Send(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Opened{
			Opened: &serverv1.Opened{
				Session:   ended.Label,
				SessionId: ended.ID,
				NextSeq:   uint64(ended.LastSeq),
			},
		},
	}); err != nil {
		return err
	}
	return srv.Send(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Exited{
			Exited: &serverv1.Exited{ExitCode: int32(ended.ExitCode)},
		},
	})
}

// isSessionOver reports whether an error from a shim means its session has ended.
//
// Matched on the message rather than with errors.Is, because ttrpc carries an error across the socket
// as a status with a string: the sentinel does not survive the trip, so the receiver has nothing to
// compare against. Ugly, and preferable to the alternative of treating every shim error as a session
// ending, which would hide real failures.
//
// The shim's own error rather than seqlog.ErrClosed, which is about an output log. Sharing one string
// made a resize on a just-exited session report "output log is closed", which the server could not
// distinguish from a genuine problem.
// isTransportClosed reports whether an error is the connection to a shim having gone away.
//
// Delegates to transport.IsClosed, which is shared with the client: the same race appears there when a
// caller asks the server to shut down and the reply loses to the connection closing.
//
// Distinct from isSessionOver, which is the shim saying its pty is gone. This is the shim not answering
// at all, which during a shutdown means it exited between accepting the request and replying -- the
// outcome the caller wanted.
func isTransportClosed(err error) bool {
	return transport.IsClosed(err)
}

// isProcessGone reports whether an error is the shell having already exited.
//
// A third shape of "the session is already over", alongside ErrSessionOver and a closed transport, and the
// only one that was not tolerated. The shim signals the shell's process group to stop it, and if the shell
// exited in the window before the shim reaped it, that call fails with ESRCH: the process is gone, which is
// what the caller asked for, but it arrived as a raw errno and a kill reported "no such process" for a
// session it had successfully disposed of.
//
// Reached most often by ending a session that was created moments earlier, which is what `cm rebind
// --replace` does to the session it moves a name off.
//
// Matched on the message because that is how it crosses the wire, which is what isSessionOver does with its
// sentinel for the same reason.
func isProcessGone(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), syscall.ESRCH.Error())
}

func isSessionOver(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), shim.ErrSessionOver.Error())
}

// anyHasPrefix reports whether any name starts with prefix.
//
// A session with no names matches nothing, which is the honest answer: `cm ls --prefix kitty.` is asking
// about names, and a session that has none is not one of them.
func anyHasPrefix(names []string, prefix string) bool {
	for _, name := range names {
		if strings.HasPrefix(name, prefix) {
			return true
		}
	}
	return false
}
