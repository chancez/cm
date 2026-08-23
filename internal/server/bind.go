package server

import (
	"context"
	"errors"
	"fmt"

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
	return &serverv1.SwitchResponse{
		Asked:      uint32(asked),
		TargetId:   targetID,
		SwitchedTo: switchTo,
		BoundName:  boundName,
	}, nil
}
