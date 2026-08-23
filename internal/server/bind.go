package server

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Bind points a name at a session.
func (s *Service) Bind(
	ctx context.Context, req *serverv1.BindRequest,
) (*serverv1.BindResponse, error) {
	if req.Name == "" {
		return nil, errors.New("no name given to bind")
	}
	if req.Session == "" {
		return nil, errors.New("no session given to bind to")
	}

	id, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}

	// Read before the write so the response can say what moved. Not a transaction, and it does not need
	// to be: the store serializes writes, and the only cost of a lost race here is a report naming a
	// session the name pointed at a moment earlier than this call thought.
	var previous string
	if existing, err := s.mgr.store.Binding(ctx, req.Name); err == nil {
		previous = existing.SessionID
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	onKill := store.KillTarget
	if req.Borrow {
		onKill = store.KillUnbind
	}
	if err := s.mgr.Bind(ctx, req.Name, id, onKill, req.Move); err != nil {
		return nil, err
	}

	return &serverv1.BindResponse{
		SessionId:         id,
		PreviousSessionId: previous,
	}, nil
}

// Unbind removes a name, leaving the session it pointed at running.
func (s *Service) Unbind(
	ctx context.Context, req *serverv1.UnbindRequest,
) (*serverv1.UnbindResponse, error) {
	if req.Name == "" {
		return nil, errors.New("no name given to unbind")
	}

	// Read first, so the response can name the session that was released. Removing a name nothing used
	// is not an error, so the absence has to be reported rather than raised.
	var id string
	if existing, err := s.mgr.store.Binding(ctx, req.Name); err == nil {
		id = existing.SessionID
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, err
	}

	removed, err := s.mgr.Unbind(ctx, req.Name)
	if err != nil {
		return nil, err
	}
	return &serverv1.UnbindResponse{Removed: removed, SessionId: id}, nil
}

// killOutcome is what killing one reference did.
type killOutcome struct {
	// unboundFrom is the session a released name had pointed at, empty when the session was killed.
	unboundFrom string
	surviving   []int32
}

// killRef kills the session a reference names, or releases the name when that is what it records.
//
// The branch lives here rather than in Manager.Kill because it is a question about the *reference*: the
// same session killed by one of its names and by its ID can mean two different things, and Kill takes an
// identity by design.
func (s *Service) killRef(
	ctx context.Context, ref string, force bool, sig int32,
) (killOutcome, error) {
	id, err := s.mgr.Resolve(ctx, ref)
	if err != nil {
		return killOutcome{}, err
	}

	// Only a name can be borrowed. An "@id" reference names the session itself, so killing by it is
	// unambiguous, which is also what makes it the way to end a session whose every name is a borrower.
	if _, isID := paths.SessionRef(ref); !isID {
		binding, err := s.mgr.store.Binding(ctx, ref)
		switch {
		case err == nil && binding.OnKill == store.KillUnbind:
			// A name that vanished between the resolve and here reports the same thing, since the
			// caller's intent is satisfied either way.
			if _, err := s.mgr.Unbind(ctx, ref); err != nil {
				return killOutcome{}, err
			}
			return killOutcome{unboundFrom: id}, nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return killOutcome{}, fmt.Errorf("reading what %q names: %w", ref, err)
		}
	}

	surviving, err := s.mgr.Kill(ctx, id, force, sig)
	if err != nil {
		return killOutcome{}, err
	}
	return killOutcome{surviving: surviving}, nil
}

// Switch points a session's client at another session, in place.
func (s *Service) Switch(
	ctx context.Context, req *serverv1.SwitchRequest,
) (*serverv1.SwitchResponse, error) {
	if req.Session == "" {
		return nil, errors.New("no session given to switch")
	}
	if req.Target == "" {
		return nil, errors.New("no target session given to switch to")
	}

	if req.Replace && !req.Bind {
		// A switch leaves the name pointing at the session being left, so ending it would leave that name
		// resolving to nothing and the next attach by it creating a session under a name that meant
		// something else an instant ago.
		return nil, errors.New("replace needs the name to move as well; use `cm rebind`")
	}

	fromID, err := s.mgr.Resolve(ctx, req.Session)
	if err != nil {
		return nil, err
	}
	targetID, err := s.mgr.Resolve(ctx, req.Target)
	if err != nil {
		return nil, err
	}
	if fromID == targetID {
		// Refused rather than done as a no-op, since a client that reattaches to the session it is already
		// showing repaints for nothing, and asking for it means the caller believes they are two sessions.
		return nil, fmt.Errorf("%s is already the session %s names",
			paths.FormatSessionID(targetID), req.Session)
	}
	// Checked before anything is asked to move, so a switch to a session that cannot be attached fails
	// with the window still where it was rather than half-way.
	if _, err := s.mgr.store.Get(ctx, targetID); err != nil {
		return nil, err
	}

	sess, live := s.mgr.Get(fromID)
	if !live {
		return nil, fmt.Errorf("session %s has no clients to switch", req.Session)
	}

	// What the clients are told to attach to, and the reason the two cases differ.
	//
	// With bind, the window's own name now points at the target, so the client is told that name. It could
	// be told the ID and land in the same place; the name is chosen because it can still recreate the
	// session if the target has gone in the moment between binding and reattaching, where an ID would only
	// fail and leave the window retrying.
	//
	// Without bind, the name still points at the session being left, so the client is told the target's
	// ID. Sending the name would send it straight back where it started.
	// Checked before anything moves, so a refusal leaves the window where it was rather than half done.
	if req.Replace && !req.Force {
		if err := s.replaceable(ctx, req, fromID); err != nil {
			return nil, err
		}
	}

	switchTo := paths.FormatSessionID(targetID)
	boundName := ""
	if req.Bind {
		names, err := s.mgr.Names(ctx, fromID)
		if err != nil {
			return nil, err
		}
		if len(names) == 0 {
			// Said plainly rather than switching for this client only. A caller asking for the durable
			// form of a switch and silently getting the ephemeral one would find out at the next restart,
			// which is the worst time.
			return nil, fmt.Errorf(
				"session %s has no name to move; bind a name to it first, or use `cm switch` to move "+
					"this window without one",
				paths.FormatSessionID(fromID))
		}
		// The name it is known by, which for a per-window session is the one the emulator put in its
		// launch command.
		boundName = names[0]
		if err := s.mgr.Bind(ctx, boundName, targetID, store.KillUnbind, true); err != nil {
			return nil, err
		}
		switchTo = boundName
	}

	asked := sess.SwitchClients(switchTo, !req.AllClients)

	resp := &serverv1.SwitchResponse{
		Asked:      uint32(asked),
		TargetId:   targetID,
		SwitchedTo: switchTo,
		BoundName:  boundName,
	}
	if req.Replace {
		resp.KilledSession, resp.KeptReason = s.replacePrevious(sess, asked)
	}
	return resp, nil
}

// replaceable reports why the session a name is moving off must not be ended, or nil when it may be.
//
// Two conditions, and neither is about the window being moved. A session with another name is something
// else's, since that name is how it is reached and nothing here asked for it to go. And a session running a
// foreground command has work in it worth more than the tidiness of removing it.
//
// The busy check is skipped when the caller is inside the session it is replacing, because there it cannot
// mean anything: `cm rebind` is itself a foreground command, so OSC 133 reports that session busy every
// time, and refusing on that would refuse always. A backgrounded job does not set it, so the shell would
// read idle in the one case where work really is running unattended. What guards the case instead is that a
// foreground command would have kept the user from typing the command at all.
func (s *Service) replaceable(ctx context.Context, req *serverv1.SwitchRequest, fromID string) error {
	names, err := s.mgr.Names(ctx, fromID)
	if err != nil {
		return err
	}
	if len(names) > 1 {
		return fmt.Errorf(
			"%s has other names (%s), so it is not this window's alone; unbind them first or pass --force",
			paths.FormatSessionID(fromID), strings.Join(names, " "))
	}

	if req.CallerSession != "" {
		if callerID, err := s.mgr.Resolve(ctx, req.CallerSession); err == nil && callerID == fromID {
			return nil
		}
	}

	sess, live := s.mgr.Get(fromID)
	if !live {
		return nil
	}
	if busy, what := sessionBusy(sess); busy {
		if what == "" {
			what = "something"
		}
		return fmt.Errorf("%s is running %s; pass --force to end it anyway, or --replace=false to keep it",
			paths.FormatSessionID(fromID), what)
	}
	return nil
}

// replacePrevious ends the session a name was moved off, once no window is watching it.
//
// Waits first, and that ordering is the whole of it: the clients were asked to move a moment ago and are
// reattaching, so ending the session before they have gone would evict them from it instead, and a window
// would exit rather than move. A window still there after the wait is one this call did not move, since
// --all-clients was not given, and taking its session away is not what was asked for.
//
// The kill runs on its own context rather than the request's. In the ordinary case the caller is the shell
// inside this session, so this kills the process waiting for the reply: on the request's context that death
// would cancel the kill halfway and leave the session running.
func (s *Service) replacePrevious(sess *Session, asked int) (killed, keptReason string) {
	if asked > 0 && !waitForNoClients(sess, clientsLeaveTimeout) {
		return "", fmt.Sprintf("a window is still attached to %s", paths.FormatSessionID(sess.id))
	}

	ctx, cancel := context.WithTimeout(context.Background(), replaceKillTimeout)
	defer cancel()
	if _, err := s.mgr.Kill(ctx, sess.id, false, 0); err != nil {
		return "", fmt.Sprintf("ending %s failed: %v", paths.FormatSessionID(sess.id), err)
	}
	return sess.id, ""
}

// waitForNoClients waits for a session to have nothing attached, reporting whether it got there.
func waitForNoClients(sess *Session, within time.Duration) bool {
	deadline := time.Now().Add(within)
	for {
		if sess.Clients() == 0 {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(clientsLeaveInterval)
	}
}

// sessionBusy reports whether a session is running something, and what.
//
// The same derivation `cm list` uses: what the shell reports through OSC 133, overridden by a program's own
// report about itself when it has made one.
func sessionBusy(sess *Session) (bool, string) {
	if r := sess.Reported(); r.State != "" {
		if r.State == "idle" {
			return false, ""
		}
		what := r.Detail
		if what == "" {
			what = r.State
		}
		return true, what
	}
	cmd := sess.Command()
	return cmd.Running, cmd.Command
}

const (
	// clientsLeaveTimeout bounds the wait for switched windows to reattach elsewhere. Generous against
	// what it waits for: a client reconnects on its own 100ms retry.
	clientsLeaveTimeout  = 5 * time.Second
	clientsLeaveInterval = 50 * time.Millisecond
	// replaceKillTimeout bounds ending the session, on a context of its own.
	replaceKillTimeout = 10 * time.Second
)
