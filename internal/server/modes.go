package server

import (
	"context"
	"log/slog"
	"slices"
	"time"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/termmodes"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// modesWriteInterval is the least time between two writes of a session's modes to the store.
//
// Some modes change every frame: nvim hides the cursor around each redraw. A write per change would be a
// sqlite write per frame. A crash loses at most this much, and nothing worse than that: a change made in the
// last interval is in the shim's retained output, which the replay sets it from.
const modesWriteInterval = time.Second

// noteModes records what the chunk starting at at did to the program's modes.
//
// Called on the pump after the model has the chunk. The model is read only when the bytes can have changed
// a mode, which most chunks cannot: about 2.9us a read, against text and cursor motion that need none.
func (s *Session) noteModes(data []byte, at seq.Shim) {
	if !s.modeWatch.MayChange(data) {
		return
	}
	s.mu.Lock()
	term := s.term
	s.mu.Unlock()
	if term == nil {
		return
	}
	values, err := term.Modes()
	if err != nil {
		s.log.Debug("reading the terminal model's modes failed", "session", s.label, "error", err)
		return
	}
	// Lazily, so a Session a test builds field by field still pumps.
	if s.modeTracker == nil {
		s.modeTracker = termmodes.NewTracker(nil)
	}
	if !s.modeTracker.Observe(values, at) {
		return
	}
	modes := s.modeTracker.Modes()
	s.mu.Lock()
	s.modes = modes
	s.mu.Unlock()
	select {
	case s.modesChanged <- struct{}{}:
	default:
	}
}

// Modes returns the modes the session's program has set that differ from a new terminal's, each with where
// it set it.
func (s *Session) Modes() []termmodes.Mode {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.modes)
}

// hasModel reports whether the session has a terminal model, which is what records its modes.
func (s *Session) hasModel() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.term != nil
}

// withModes hands the modes an adoption rebuilt to the session, so the pump carries on from them: an
// unchanged mode keeps the position that set it, which is what the next adoption compares.
func withModes(tracker *termmodes.Tracker) sessionOpt {
	return func(s *Session) {
		if tracker == nil {
			return
		}
		s.modeTracker = tracker
		s.modes = tracker.Modes()
	}
}

// adoptModes applies to a model about to be fed a shim's retained output the recorded modes the output no
// longer holds, and returns the tracker the replay continues.
//
// oldest is the first byte the model will be fed, by the replay or, when nothing before the resume point is
// retained, by the pump. The modes set before it are the ones nothing will set. See termmodes.Trimmed for
// why the rest are left to the replay.
func adoptModes(log *slog.Logger, rec store.Session, term Terminal, oldest seq.Shim) *termmodes.Tracker {
	trimmed := termmodes.Trimmed(rec.Modes, oldest)
	if len(trimmed) == 0 {
		return termmodes.NewTracker(nil)
	}
	if err := term.ApplyModes(termmodes.Values(trimmed)); err != nil {
		log.Warn("applying the modes a replay no longer holds failed", "session", rec.ID, "error", err)
		return termmodes.NewTracker(nil)
	}
	// Said, because it is the state a restored screen depends on and nothing else shows it: without this, a
	// gr three days old came back on the main screen with no mouse reporting.
	log.Info("applied terminal modes the shim no longer holds the output for", "session", rec.ID,
		"modes", termmodes.Format(trimmed), "oldest_seq", uint64(oldest))
	return termmodes.NewTracker(trimmed)
}

// persistModes writes the session's modes to the store whenever they change, at most once per
// modesWriteInterval, until the session ends.
//
// stored is what the record already says, so an adoption that rebuilt exactly that writes nothing, and one
// that rebuilt something else, a revive that reset them or a replay that saw them change, writes it once.
// Manager.Close writes them again with the resume points, so a clean restart loses nothing to the interval.
func (m *Manager) persistModes(sess *Session, stored []termmodes.Mode) {
	last := stored
	write := func() {
		modes := sess.Modes()
		if slices.Equal(modes, last) {
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.store.Apply(ctx, sess.id, store.Update{Modes: &modes}); err != nil {
			m.log.Warn("recording terminal modes failed", "session", sess.id, "error", err)
			return
		}
		last = modes
	}
	write()
	for {
		select {
		case <-sess.modesChanged:
			write()
		case <-sess.Done():
			return
		}
		// Changes made meanwhile wait in modesChanged, coalesced, for the next write.
		select {
		case <-time.After(modesWriteInterval):
		case <-sess.Done():
			return
		}
	}
}

// modesProto converts recorded modes for the wire.
func modesProto(modes []termmodes.Mode) []*serverv1.TerminalMode {
	if len(modes) == 0 {
		return nil
	}
	out := make([]*serverv1.TerminalMode, len(modes))
	for i, m := range modes {
		// The one crossing of the shim's numbering onto the wire, as last_seq makes.
		out[i] = &serverv1.TerminalMode{Key: m.Key, Value: int32(m.Value), SetAt: uint64(m.SetAt)}
	}
	return out
}
