package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"time"

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
		Owned:     open.Own,
		ClientEnv: open.ClientEnv,
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
		_ = s.mgr.store.Apply(ctx, sess.name, store.Update{Env: open.ClientEnv})
	}

	// Resize before snapshotting, not after.
	//
	// The order matters and is easy to get backwards. A snapshot taken at the session's old size
	// describes lines wrapped for that width; a client of a different width then wraps them
	// again, and the screen arrives visibly mangled. Resizing first means the model reflows once,
	// and the snapshot describes what this client will actually display.
	//
	// A newly attached client's size wins, so the shell matches the terminal showing it. On
	// resume the client already matches, and resizing would make the shell redraw for no reason.
	resizing := open.ResumeFromSeq == nil && !open.ReadOnly && open.Rows > 0 && open.Cols > 0
	if resizing {
		// ResizeSignal rather than Resize: on a fresh attach the shell has to redraw even when the
		// size is unchanged, or a program that repaints only on SIGWINCH keeps updating a screen
		// that is now the snapshot replayed below.
		if err := sess.ResizeSignal(ctx, open.Rows, open.Cols, open.XPixel, open.YPixel); err != nil {
			return fmt.Errorf("sizing session %s: %w", sess.name, err)
		}
		rows, cols := int(open.Rows), int(open.Cols)
		_ = s.mgr.store.Apply(ctx, sess.name, store.Update{Rows: &rows, Cols: &cols})
	}

	att, err := sess.attach(open.ResumeFromSeq)
	if err != nil {
		return err
	}
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
		recvErr <- s.recvLoop(ctx, sess, srv, open.ReadOnly, detached, &sawDetach)
	}()

	// reapIfAbandoned ends an owned session whose client vanished without detaching.
	reapIfAbandoned := func() {
		if open.Own && !sawDetach.Load() {
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
					ended, code := sess.Ended()
					if ended {
						return srv.Send(&serverv1.AttachResponse{
							Event: &serverv1.AttachResponse_Exited{
								Exited: &serverv1.Exited{ExitCode: int32(code)},
							},
						})
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
			if err := sess.Write(ctx, req.GetInput().Data); err != nil {
				return fmt.Errorf("writing to session %s: %w", sess.name, err)
			}

		case req.GetResize() != nil:
			if readOnly {
				continue
			}
			r := req.GetResize()
			if err := sess.Resize(ctx, r.Rows, r.Cols, r.XPixel, r.YPixel); err != nil {
				return fmt.Errorf("resizing session %s: %w", sess.name, err)
			}
			rows, cols := int(r.Rows), int(r.Cols)
			_ = s.mgr.store.Apply(ctx, sess.name, store.Update{Rows: &rows, Cols: &cols})
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
	_ = s.mgr.Kill(ctx, sess.name, true)
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
	sess, ok := s.mgr.Get(req.Session)
	if !ok {
		return nil, fmt.Errorf("%q: %w", req.Session, store.ErrNotFound)
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
