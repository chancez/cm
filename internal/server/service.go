package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/tags"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

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

	sess, created, err := s.mgr.Open(ctx, OpenOptions{
		Name:          open.Session,
		Rows:          uint16(open.Rows),
		Cols:          uint16(open.Cols),
		Command:       open.Command,
		Dir:           open.Cwd,
		Env:           open.Env,
		Owned:         open.Own && !open.ReadOnly,
		ClientEnv:     open.ClientEnv,
		Persist:       open.Persist,
		CaptureOutput: open.CaptureOutput,
		OnRestore:     RestoreAction(open.OnRestore),
		Tags:          open.Tags,
	})
	if err != nil {
		return err
	}

	// Record what this client's terminal looks like, on every attach rather than only on
	// creation. A shell that has been running for days holds values describing a terminal that
	// may have been replaced since, and this is the only record of the current one.
	//
	// After Open rather than before, because a session created by this call has no row to update
	// until then.
	if len(open.ClientEnv) > 0 {
		if err := s.mgr.store.Apply(ctx, sess.name, store.Update{Env: open.ClientEnv}); err != nil {
			// Advisory: the session works, but `get-env` will hand out stale values, which is
			// otherwise mysterious.
			s.mgr.log.Warn("recording client environment failed",
				"session", sess.name, "error", err)
		}
	}

	// Resize before snapshotting, not after.
	//
	// The order matters and is easy to get backwards. A snapshot taken at the session's old size
	// describes lines wrapped for that width; a client of a different width then wraps them
	// again, and the screen arrives visibly mangled. Resizing first means the model reflows once,
	// and the snapshot describes what this client will actually display.
	//
	// A client that wants the screen repainted is displaying the session, which is what distinguishes it
	// from one attaching only to create the session or to stream bytes. Recorded here rather than derived
	// later, since the request is the only place the distinction exists.
	if !open.NoRestore {
		sess.noteWatched()
	}

	att, err := sess.attach(open.ResumeFromSeq)
	if err == nil && open.ReadOnly {
		// Recorded here rather than only in registerClientSize below, which a resuming client skips.
		// A follower cannot answer a terminal query, since its input is dropped, and counting one as
		// an answerer makes the emulator stay silent and the querying program hang.
		sess.markReadOnly(att.token)
	}
	if err != nil {
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
						Session: sess.name,
						Created: created,
						NextSeq: sess.LastSeq(),
					},
				},
			}); err != nil {
				return err
			}
			return s.sendExited(srv, sess)
		}
		return err
	}

	// Whether this client's size wins depends on the configured policy, which only matters once
	// several clients are attached. On resume the client already matches, so nothing is resized and
	// the shell is not made to redraw for no reason.
	if open.ResumeFromSeq == nil {
		rows, cols, x, y, resize := sess.registerClientSize(
			att.token, uint16(open.Rows), uint16(open.Cols),
			uint16(open.XPixel), uint16(open.YPixel), open.ReadOnly,
		)
		if resize {
			// ResizeSignal rather than Resize: on a fresh attach the shell has to redraw even when
			// the size is unchanged, or a program that repaints only on SIGWINCH keeps updating a
			// screen that is now the snapshot replayed below.
			if err := sess.ResizeSignal(ctx, uint32(rows), uint32(cols), uint32(x), uint32(y)); err != nil {
				// A shim that has gone away is not a sizing failure, it is the session ending, and
				// the same race as above one step later: attach succeeded, then the shell exited
				// before this resize reached the shim. Sizing a session that no longer exists is
				// moot, so the attach continues and the loop below reports the exit status.
				//
				// Without this, `cm run` on a fast command failed with "sizing session s9: ttrpc:
				// closed" instead of the command's exit code, about one run in ten on Linux.
				//
				// Two ways to recognize it, because they can arrive in either order. The session may
				// already be marked ended, or the shim may say so first: the server learns of an exit
				// by watching the output stream, so a resize can reach a shim whose pty is already
				// released while this session still looks alive.
				if ended, _ := sess.Ended(); !ended && !isSessionOver(err) {
					sess.detach(att)
					return fmt.Errorf("sizing session %s: %w", sess.name, err)
				}
				s.mgr.log.Info("skipped sizing a session that ended while attaching",
					"session", sess.name)
			}
			ir, ic := int(rows), int(cols)
			if err := s.mgr.store.Apply(ctx, sess.name, store.Update{Rows: &ir, Cols: &ic}); err != nil {
				s.mgr.log.Warn("recording session size failed", "session", sess.name, "error", err)
			}
		}
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
	if open.InsideSession != "" && open.InsideSession != sess.name {
		if parent, live := s.mgr.Get(open.InsideSession); live {
			parent.beginHosting(sess.name)
			defer parent.endHosting(sess.name)
		}
	}

	s.mgr.log.Info("client attached",
		"session", sess.name, "created", created, "resuming", open.ResumeFromSeq != nil,
		"read_only", open.ReadOnly, "owns", open.Own && !open.ReadOnly,
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
				Session: sess.name,
				Created: created,
				Restore: restore,
				NextSeq: startSeq,
			},
		},
	}); err != nil {
		return err
	}

	// Whether a Detach was seen is the whole basis of session ownership: closing a terminal
	// window ends an owned session, while detaching leaves it running.
	//
	// This is tracked as a flag rather than inferred from how the stream ended. A dropped
	// connection surfaces as both a receive error and a cancelled request context, racing each
	// other, so the exit path cannot tell a detach from a disconnect on its own.
	var sawDetach atomic.Bool
	detached := make(chan struct{})
	recvErr := make(chan error, 1)
	go func() {
		recvErr <- s.recvLoop(ctx, sess, srv, open.ReadOnly, att.token, detached, &sawDetach)
	}()

	// reapIfAbandoned ends an owned session whose client vanished without detaching.
	//
	// A read-only client can never trigger this, however it asked. Ownership means "this session
	// exists for my window", which contradicts watching someone else's, and honoring both flags
	// together would let a follower destroy the session it was only observing.
	owns := open.Own && !open.ReadOnly
	reapIfAbandoned := func() {
		if owns && !sawDetach.Load() {
			s.reapOwned(sess)
		}
	}

	// Metadata is forwarded so a terminal emulator can retitle a tab or open a new window in the
	// session's directory. Subscribing before the loop means the current values arrive
	// immediately rather than only on the next change.
	metaSub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(metaSub)

	// Output is read on its own goroutine because the reader blocks, and this loop also has
	// to notice a detach or a dropped connection.
	type chunkMsg struct {
		chunk seqlog.Chunk
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
						Seq:  msg.chunk.Seq,
						Data: msg.chunk.Data,
						Gap:  msg.chunk.Gap,
					},
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
			// Deliberate detach: the session keeps running even if this client owns it.
			return nil

		case err := <-recvErr:
			reapIfAbandoned()
			if err != nil && !errors.Is(err, context.Canceled) {
				return err
			}
			return nil

		case <-ctx.Done():
			// The request context is cancelled when the connection drops, which races with the
			// receive loop failing. Both have to honor ownership, or an owned session would
			// survive a closed window roughly half the time depending on which fired first.
			reapIfAbandoned()
			return ctx.Err()
		}
	}
}

// recvLoop handles client-to-server events until the stream ends.
func (s *Service) recvLoop(
	ctx context.Context,
	sess *Session,
	srv serverv1.Server_AttachServer,
	readOnly bool,
	tok *attachToken,
	detached chan<- struct{},
	sawDetach *atomic.Bool,
) error {
	for {
		req, err := srv.Recv()
		if err != nil {
			return err
		}

		switch {
		case req.GetDetach() != nil:
			// Recorded before signalling, so the exit path cannot observe the close without also
			// seeing the flag.
			sawDetach.Store(true)
			// Acknowledged before signalling, so the client can wait for confirmation rather than
			// racing its own disconnect. A client that sent Detach and exited immediately had the
			// message dropped, because the send is asynchronous and closing the connection discarded
			// it, and the server then reaped the owned session it was asked to keep.
			//
			// Skipped when the client said it will not wait. `cm run -d`, `cm attach --no-attach`, and an
			// interrupted follower all detach as their last act and exit; their connection is closing as the
			// Detach arrives, so the reply lost a race about 40% of the time and produced a warning for
			// behavior that was intended. The session was never at risk, since sawDetach above is what
			// protects it, so the warning was noise that also made `cm doctor` report a healthy installation
			// as having a problem.
			if !req.GetDetach().NoAck {
				if err := srv.Send(&serverv1.AttachResponse{
					Event: &serverv1.AttachResponse_Detached{Detached: &serverv1.Detached{}},
				}); err != nil {
					// A client that asked for the acknowledgement and then vanished before it arrived. Worth
					// a warning, unlike the case above: it means an interactive client lost its connection
					// mid-detach, and for an owned session the flag above is the only thing that saved it.
					s.mgr.log.Warn("acknowledging a detach failed",
						"session", sess.name, "error", err)
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
			if !input.IsUserInput(req.GetInput().Data) {
				// A reply to a terminal query, a mouse report, or a focus change. Forwarded to the
				// shell, since the program asked for it, but it must not claim sizing: otherwise a
				// window nobody is using takes over because the program polled the terminal.
				//
				// Except that a query reply is forwarded from one client only. Output fans out to every
				// attached client, so two terminals both see a query and both answer it: measured with
				// two clients on one session, a single CSI c came back as "\x1b[?62;52;c\x1b[?62;52;c".
				// The program reads one and the spare is left for the shell's line editor, which is the
				// artifact that printed "62;52;c" beside a prompt.
				//
				// Only replies are dropped, not everything non-typing. Mouse and focus events describe
				// one window rather than the session, so each client sends its own and dropping them
				// would make a session ignore the mouse in every window but one.
				if input.IsQueryReply(req.GetInput().Data) && !sess.isAnswerer(tok) {
					continue
				}
				if err := sess.Write(ctx, req.GetInput().Data); err != nil {
					return fmt.Errorf("writing to session %s: %w", sess.name, err)
				}
				continue
			}
			// Typing may transfer sizing, depending on the policy. Checked before the write so
			// the shell is already at the right size when it sees the keystroke.
			if rows, cols, x, y, resize := sess.claimLeadership(tok); resize {
				if err := sess.Resize(ctx, uint32(rows), uint32(cols), uint32(x), uint32(y)); err != nil {
					s.mgr.log.Warn("resizing on leadership change failed",
						"session", sess.name, "error", err)
				} else {
					ir, ic := int(rows), int(cols)
					if err := s.mgr.store.Apply(ctx, sess.name,
						store.Update{Rows: &ir, Cols: &ic}); err != nil {
						s.mgr.log.Warn("recording session size failed",
							"session", sess.name, "error", err)
					}
				}
			}
			if err := sess.Write(ctx, req.GetInput().Data); err != nil {
				return fmt.Errorf("writing to session %s: %w", sess.name, err)
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
				return fmt.Errorf("resizing session %s: %w", sess.name, err)
			}
			ir, ic := int(rows), int(cols)
			if err := s.mgr.store.Apply(ctx, sess.name, store.Update{Rows: &ir, Cols: &ic}); err != nil {
				s.mgr.log.Warn("recording session size failed", "session", sess.name, "error", err)
			}
		}
	}
}

// reapOwned ends a session whose owning client vanished.
//
// Uses a fresh context because the request context is already cancelled: it was the client
// going away that triggered this.
func (s *Service) reapOwned(sess *Session) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	s.mgr.log.Info("owning client vanished, ending session", "session", sess.name)
	if _, err := s.mgr.Kill(ctx, sess.name, true, 0); err != nil {
		// A session that should have been reaped and was not becomes a leak the user has to notice
		// on their own, so it is logged even though nothing can be done here.
		s.mgr.log.Error("reaping owned session failed", "session", sess.name, "error", err)
	}
}

func (s *Service) List(ctx context.Context, req *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	// Parsed before the query so a malformed selector is an error rather than a silent match of
	// everything, which would be the wrong answer for `cm kill --tag`.
	selector, err := tags.ParseSelector(req.Tags)
	if err != nil {
		return nil, err
	}

	records, err := s.mgr.List(ctx, req.Prefix)
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
		item := &serverv1.Session{
			Name:          rec.Name,
			ShellPid:      int32(rec.ShellPID),
			Clients:       uint32(s.mgr.Clients(rec.Name)),
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
		if sess, live := s.mgr.Get(rec.Name); live {
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
	for _, name := range req.Sessions {
		surviving, err := s.mgr.Kill(ctx, name, req.Force, req.Signal)
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

func (s *Service) Send(ctx context.Context, req *serverv1.SendRequest) (*serverv1.SendResponse, error) {
	sess, ok := s.mgr.Get(req.Session)
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
		if err := sess.Write(ctx, req.Data); err != nil {
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

	if err := sess.Write(ctx, req.Data); err != nil {
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
func (s *Service) readableSession(name string) *Session {
	sess, live := s.mgr.Get(name)
	if !live {
		return nil
	}
	if ended, _ := sess.Ended(); ended {
		return nil
	}
	return sess
}

func (s *Service) History(ctx context.Context, req *serverv1.HistoryRequest) (*serverv1.HistoryResponse, error) {
	sess := s.readableSession(req.Session)
	if sess == nil {
		// A session that has ended has no terminal model, whether or not it has left the registry yet. If it
		// was persisting, its output is still on disk and can be replayed, which is what makes reading a
		// finished command's output possible at all.
		data, err := s.mgr.HistoryFromDisk(ctx, req.Session, req.Format)
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

	sess := s.readableSession(req.Session)
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
		data, err := s.mgr.ReadFromDisk(ctx, req.Session, int(req.Lines), req.Unwrap, req.Raw)
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
		from uint64
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
	rec, err := s.mgr.store.Get(ctx, req.Session)
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

func isSessionOver(err error) bool {
	if err == nil {
		return false
	}
	return strings.Contains(err.Error(), shim.ErrSessionOver.Error())
}
