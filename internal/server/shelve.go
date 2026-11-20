package server

import (
	"context"
	"errors"
	"fmt"
	"syscall"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/shim"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// ShelveOutcome is what shelving one session did.
type ShelveOutcome int

const (
	// Shelved means the session is now recorded as shelved: its shell was ended, or it had
	// already ended and was marked.
	Shelved ShelveOutcome = iota + 1
	// AlreadyShelved means there was nothing to do.
	AlreadyShelved
)

// ErrNothingSaved refuses to shelve a session that saved no output.
var ErrNothingSaved = errors.New("saved no output, so there is nothing to restore it to")

// shelveExitWait bounds how long Shelve waits for a session's processes to exit after the hangup,
// and shelveKillWait how long after the SIGKILL that follows.
//
// Waited for rather than assumed, because the shim stops serving shortly after it answers a Shutdown,
// whether or not the shell has gone: zsh printing "zsh: hangup" and drawing a prompt on the way out took
// longer than that, so `cm kill --keep` recorded such a session as dead with no status. A shelved
// session has to be one whose processes are gone, or a restore starts a second copy of a program still
// holding its files. Three seconds is generous against a shell that answers the hangup, and short enough
// that one ignoring it does not stall shelving several sessions.
const (
	shelveExitWait = 3 * time.Second
	shelveKillWait = 2 * time.Second
	shelvePoll     = 50 * time.Millisecond
)

// Shelve ends a session deliberately so it can be restored as it was.
//
// Everything a restore needs is kept: the record, names, tags, saved output and the declared resume command,
// which is frozen as it was when the shelving began, through the same flag `cm kill --keep` uses, since
// the hangup closes the program's frame on the way out. Not SIGTSTP: nothing is stopped and resumed, the
// processes end, and a restore starts them again from what was kept.
//
// expiry is the retention the shelved record gets, and zero means never: a shelved session is kept
// until it is restored, forgotten or killed, unless the caller said otherwise.
//
// A session that saved no output is refused, since a restore would come back to an empty screen and a fresh
// shell, which is a new session wearing an old name. One that had already ended is marked shelved as it
// stands, keeping its declaration.
func (m *Manager) Shelve(ctx context.Context, id string, expiry store.Expiry) (ShelveOutcome, error) {
	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return 0, err
	}
	if rec.State == store.StateShelved {
		return AlreadyShelved, nil
	}
	if rec.LogPath == "" || m.persist == nil {
		return 0, fmt.Errorf("%w; only a session that persists, with --persist or by persist.sessions, "+
			"can be shelved", ErrNothingSaved)
	}
	if expiry == 0 {
		expiry = store.ExpireNever
	}

	sess, live := m.Get(id)
	if live || rec.State == store.StateRunning {
		pids := []int{}
		if live {
			// Before the kill, so watch() records what this is when the shell dies. See
			// Session.shelving.
			sess.shelving.Store(true)
			// The pid from the shim rather than the record, which an adopted session may never have had
			// written: recordShellPID runs only when a session is created.
			if st, err := sess.State(ctx); err == nil && st.ShellPid > 0 {
				pids = append(pids, int(st.ShellPid))
			}
		}
		if len(pids) == 0 && rec.ShellPID > 0 {
			pids = append(pids, rec.ShellPID)
		}
		m.log.Info("shelving session", "session", id, "live", live, "expire_after", expiry.String())
		surviving, err := m.Kill(ctx, id, KillOptions{Keep: true})
		if err != nil {
			if live {
				sess.shelving.Store(false)
			}
			return 0, err
		}
		for _, pid := range surviving {
			pids = append(pids, int(pid))
		}
		if err := m.awaitExit(ctx, id, pids); err != nil {
			return 0, err
		}
	}

	state, code := store.StateShelved, 0
	if err := m.store.Apply(ctx, id, store.Update{State: &state, ExitCode: &code, ExpireAfter: &expiry}); err != nil {
		return 0, fmt.Errorf("recording %s as shelved: %w", id, err)
	}
	m.log.Info("shelved session", "session", id, "previous_state", rec.State,
		"expire_after", expiry.String())
	return Shelved, nil
}

// awaitExit waits for a shelving session's processes to be gone, killing what outlives the hangup.
//
// pids is the shell and whatever the shim reported as surviving the signal. Gone means ProcessLives says
// so, which counts a zombie as gone: the shell is the shim's child, and the shim may exit before reaping
// it.
func (m *Manager) awaitExit(ctx context.Context, id string, pids []int) error {
	left, err := waitGone(ctx, pids, shelveExitWait)
	if err != nil || len(left) == 0 {
		return err
	}
	// Escalated by pid rather than through the shim, which has usually stopped serving by now. Each of
	// these was alive at the last poll, so the window for a pid to have been reused is one poll wide.
	m.log.Warn("processes outlived the hangup while shelving; killing them",
		"session", id, "pids", left, "waited", shelveExitWait)
	for _, pid := range left {
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	if left, err = waitGone(ctx, left, shelveKillWait); err != nil {
		return err
	}
	if len(left) > 0 {
		return fmt.Errorf("shelving %s: processes %v survived SIGKILL; see `%s doctor`", id, left, paths.Name)
	}
	return nil
}

// waitGone polls until none of pids is a live process, or the bound passes, and returns what is left.
func waitGone(ctx context.Context, pids []int, bound time.Duration) ([]int, error) {
	deadline := time.Now().Add(bound)
	for {
		var left []int
		for _, pid := range pids {
			if shim.ProcessLives(pid) {
				left = append(left, pid)
			}
		}
		if len(left) == 0 || time.Now().After(deadline) {
			return left, nil
		}
		pids = left
		select {
		case <-ctx.Done():
			return left, ctx.Err()
		case <-time.After(shelvePoll):
		}
	}
}

// Shelve ends sessions deliberately so they can be restored as they were. See Manager.Shelve.
func (s *Service) Shelve(
	ctx context.Context, req *serverv1.ShelveRequest,
) (*serverv1.ShelveResponse, error) {
	// For the whole request, as Kill checks its retention: the same value applies to every session named.
	var expiry store.Expiry
	if req.ExpireAfter != "" {
		var err error
		if expiry, err = store.ParseExpiry(req.ExpireAfter); err != nil {
			return nil, err
		}
	}
	resp := &serverv1.ShelveResponse{Errors: make(map[string]string)}
	for _, ref := range req.Sessions {
		id, err := s.mgr.Resolve(ctx, ref)
		if err != nil {
			resp.Errors[ref] = trimNamePrefix(err.Error(), ref)
			continue
		}
		outcome, err := s.mgr.Shelve(ctx, id, expiry)
		switch {
		case err != nil:
			resp.Errors[ref] = trimNamePrefix(err.Error(), id)
		case outcome == AlreadyShelved:
			resp.AlreadyShelved = append(resp.AlreadyShelved, ref)
		default:
			resp.Shelved = append(resp.Shelved, ref)
		}
	}
	if len(resp.Errors) == 0 {
		resp.Errors = nil
	}
	return resp, nil
}
