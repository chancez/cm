package server

import (
	"context"
	"os"
	"time"

	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// startedAt is when this process began serving.
//
// Captured at package init rather than passed in, because the only caller that could supply it is the command
// that starts the server, and threading a timestamp through the manager for one diagnostic reply is more
// plumbing than the fact is worth. The difference between init and the moment the socket is bound is
// milliseconds, which does not matter for an uptime measured in days.
var startedAt = time.Now()

// Status reports facts about this server, for the RPC.
//
// Facts rather than problems, which is what separates it from Doctor. Doctor runs fifteen checks, several of
// which dial every shim and read log files; asking it for a pid would do all of that work to answer something
// cheap. This reads the registry and one store query.
func (s *Service) Status(
	ctx context.Context, _ *serverv1.StatusRequest,
) (*serverv1.StatusResponse, error) {
	resp := &serverv1.StatusResponse{
		Pid:           int32(os.Getpid()),
		StartedAtUnix: startedAt.Unix(),
		Version:       s.mgr.Version(),
		RuntimeDir:    s.mgr.dirs.Runtime,
		StateDir:      s.mgr.dirs.State,
		// A property of this server, not of the client asking: a server built without the emulator cannot
		// restore a screen however capable the client is.
		Terminal: s.mgr.newTerminal != nil,
	}

	// Live clients from the registry, since only this process knows them. The store records sessions, not
	// who is attached.
	s.mgr.mu.Lock()
	for _, sess := range s.mgr.sessions {
		resp.Clients += int32(sess.Clients())
	}
	s.mgr.mu.Unlock()

	// Counts from the store rather than the registry, so finished sessions are included: the registry holds
	// only what this server is proxying, and a record left by an earlier one still shows in `cm list`.
	records, err := s.mgr.store.List(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, rec := range records {
		switch rec.State {
		case store.StateRunning:
			resp.SessionsRunning++
		case store.StateExited:
			resp.SessionsExited++
		default:
			// Dead, and anything a later version adds. Counted rather than dropped, so a total computed from
			// these fields matches what `cm list` shows.
			resp.SessionsDead++
		}
	}

	return resp, nil
}
