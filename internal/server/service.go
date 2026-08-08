package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

	"github.com/chancez/cm/internal/input"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Service adapts a Manager to the client-facing ttrpc API.
type Service struct {
	mgr *Manager
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

	sess, created, err := s.mgr.Open(ctx, OpenOptions{
		Name:      open.Session,
		Rows:      uint16(open.Rows),
		Cols:      uint16(open.Cols),
		Command:   open.Command,
		Dir:       open.Cwd,
		Env:       open.Env,
		Owned:     open.Own && !open.ReadOnly,
		ClientEnv: open.ClientEnv,
		Persist:   open.Persist,
		OnRestore: RestoreAction(open.OnRestore),
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
	att, err := sess.attach(open.ResumeFromSeq)
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
				if ended, _ := sess.Ended(); !ended {
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
	s.mgr.log.Info("client attached",
		"session", sess.name, "created", created, "resuming", open.ResumeFromSeq != nil,
		"read_only", open.ReadOnly, "owns", open.Own && !open.ReadOnly,
		"restore_bytes", len(att.restore))
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

	if err := srv.Send(&serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Opened{
			Opened: &serverv1.Opened{
				Session: sess.name,
				Created: created,
				Restore: att.restore,
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
	if err := s.mgr.Kill(ctx, sess.name, true); err != nil {
		// A session that should have been reaped and was not becomes a leak the user has to notice
		// on their own, so it is logged even though nothing can be done here.
		s.mgr.log.Error("reaping owned session failed", "session", sess.name, "error", err)
	}
}

func (s *Service) List(ctx context.Context, req *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	records, err := s.mgr.List(ctx, req.Prefix)
	if err != nil {
		return nil, err
	}

	out := &serverv1.ListResponse{Sessions: make([]*serverv1.Session, 0, len(records))}
	for _, rec := range records {
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

			title, cwd := sess.Metadata()
			if title != "" {
				item.Title = title
			}
			if cwd.Path != "" {
				item.Cwd = cwd.Path
				item.CwdIsLocal = cwd.IsLocal
			}
			item.CwdUri = sess.CwdURI()
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
		if err := s.mgr.Kill(ctx, name, req.Force); err != nil {
			// The map is already keyed by name, so a message that repeats it reads as
			// `nosuch: "nosuch": session not found`. Strip the redundant prefix.
			resp.Errors[name] = trimNamePrefix(err.Error(), name)
			continue
		}
		resp.Killed = append(resp.Killed, name)
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
	if err := sess.Write(ctx, req.Data); err != nil {
		return nil, err
	}
	return &serverv1.SendResponse{}, nil
}

func (s *Service) History(ctx context.Context, req *serverv1.HistoryRequest) (*serverv1.HistoryResponse, error) {
	sess, live := s.mgr.Get(req.Session)
	if !live {
		// A session that has ended leaves the registry, so its terminal model is gone. If it was
		// persisting, its output is still on disk and can be replayed, which is what makes reading a
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
