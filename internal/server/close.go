package server

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// closeFacts is what deciding a close needs to know about a running session.
type closeFacts struct {
	// others is how many clients still show the session once the closed window's has been let go.
	others int
	// promptSeen is whether the shell has ever sent an OSC 133 marker. Without one nothing says the shell is
	// idle, so a shell with no integration running vim reads exactly like one at a prompt.
	promptSeen bool
	// running is OSC 133's "a command is executing".
	running bool
	// framed is whether a command frame is open. Checked as well as running because OSC 133 is not enough
	// on its own: a remote shell's prompt markers arriving through a local `ssh` read as the local shell
	// back at a prompt, while the frame the local shell opened for the ssh stays open until it returns.
	framed bool
	// reportedBusy is a program's own report of working or blocked.
	reportedBusy bool
}

// closeAction is what a close does to a session nothing else is showing.
type closeAction int

const (
	// closeDetach leaves the session running.
	closeDetach closeAction = iota
	// closeEnd ends the session: shelved when it saved output, killed when there is nothing to shelve to.
	closeEnd
	// closeKill ends the session and forgets it.
	closeKill
	// closeKeep does nothing more, since another window still shows the session.
	closeKeep
)

// decideClose is the close policy. Nothing is ended while another window shows the session. By default only
// an idle shell is ended, since a long job or an agent is exactly what a window is closed on to be found
// again later in its space; policy can end either kind outright, for someone who closes a window only to be
// done with it.
func decideClose(f closeFacts, policy ClosePolicy) closeAction {
	switch {
	case f.others > 0:
		return closeKeep
	case !f.promptSeen, f.running, f.framed, f.reportedBusy:
		if policy.KillBusy {
			return closeKill
		}
		return closeDetach
	case policy.KillIdle:
		return closeKill
	}
	return closeEnd
}

// Close applies the close policy to the session a closed window showed.
func (s *Service) Close(ctx context.Context, req *serverv1.CloseRequest) (*serverv1.CloseResponse, error) {
	if req.Session == "" {
		return nil, errors.New("a session name is required")
	}
	outcome, err := s.closeRef(ctx, req.Session, req.ClientPid)
	if err != nil {
		return nil, err
	}
	slog.Info("closed", "session", req.Session, "client_pid", req.ClientPid, "outcome", outcome.String())
	return &serverv1.CloseResponse{Outcome: outcome}, nil
}

func (s *Service) closeRef(ctx context.Context, ref string, clientPID int32) (serverv1.CloseOutcome, error) {
	// A window whose name points to another host lets go of the name, as one that borrowed a session here
	// does. Its client was on the other host, so there is nothing here to detach.
	if target, err := s.mgr.unbindRemote(ctx, ref); err != nil {
		return 0, err
	} else if target != "" {
		return serverv1.CloseOutcome_CLOSE_OUTCOME_UNBOUND, nil
	}
	id, err := s.mgr.Resolve(ctx, ref)
	if err != nil {
		return 0, err
	}
	sess, live := s.mgr.Get(id)

	// A window that borrowed a session by name lets go of the name, as cm kill by that name does, and never
	// ends the session: whoever created it elsewhere still wants it. Only a name can be borrowed; see
	// killRef.
	if _, isID := paths.SessionRef(ref); !isID {
		binding, err := s.mgr.store.Binding(ctx, ref)
		switch {
		case err == nil && binding.OnKill == store.KillUnbind:
			if live {
				evictClosed(sess, clientPID)
			}
			if _, err := s.mgr.Unbind(ctx, ref); err != nil {
				return 0, err
			}
			return serverv1.CloseOutcome_CLOSE_OUTCOME_UNBOUND, nil
		case err != nil && !errors.Is(err, store.ErrNotFound):
			return 0, fmt.Errorf("reading what %q names: %w", ref, err)
		}
	}

	if !live {
		return serverv1.CloseOutcome_CLOSE_OUTCOME_NOT_RUNNING, nil
	}
	facts := closeFacts{
		others:       evictClosed(sess, clientPID),
		promptSeen:   sess.PromptSeen(),
		running:      sess.Command().Running,
		framed:       len(sess.Location()) > 0,
		reportedBusy: sess.Reported().Busy(),
	}
	switch decideClose(facts, s.mgr.closePolicy) {
	case closeKeep:
		return serverv1.CloseOutcome_CLOSE_OUTCOME_KEPT, nil
	case closeDetach:
		return serverv1.CloseOutcome_CLOSE_OUTCOME_DETACHED, nil
	case closeKill:
		if _, err := s.mgr.Kill(ctx, id, KillOptions{}); err != nil {
			return 0, err
		}
		return serverv1.CloseOutcome_CLOSE_OUTCOME_KILLED, nil
	}
	return s.endIdle(ctx, id)
}

// evictClosed lets go of the closed window's client, or of every client when no pid was given, and returns
// how many others remain.
func evictClosed(sess *Session, clientPID int32) int {
	if clientPID == 0 {
		sess.EvictClients()
		return 0
	}
	_, remaining := sess.EvictClientPID(clientPID)
	return remaining
}

// endIdle ends a session whose shell is idle at a prompt: shelved when it saved output, so it can be
// restored as it was, and killed when it saved none.
//
// Killed rather than left running in that case because an idle shell holds nothing a kill loses except its
// scrollback, and leaving every closed window's shell running is the pile of sessions spaces exist to end.
// Persistence is off by default, so this is the common case on a plain install.
//
// Shelved with the configured expire_after rather than shelve's own never, since nobody chose to keep this
// one: a week of closed windows would otherwise stay shelved for good.
func (s *Service) endIdle(ctx context.Context, id string) (serverv1.CloseOutcome, error) {
	var expiry store.Expiry
	if p := s.mgr.persist; p != nil && p.ExpireAfter > 0 {
		expiry = store.Expiry(p.ExpireAfter)
	}
	_, err := s.mgr.Shelve(ctx, id, expiry)
	switch {
	case err == nil:
		return serverv1.CloseOutcome_CLOSE_OUTCOME_SHELVED, nil
	case !errors.Is(err, ErrNothingSaved):
		return 0, err
	}
	if _, err := s.mgr.Kill(ctx, id, KillOptions{}); err != nil {
		return 0, err
	}
	return serverv1.CloseOutcome_CLOSE_OUTCOME_KILLED, nil
}
