package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// Turning what a caller typed into the session it meant.
//
// Every command takes a reference, which is either a name or an "@id", and everything below this layer
// takes an ID. Keeping the translation in one place is not tidiness: a handler that looked a name up in
// the registry directly would find nothing and report the session as absent, and a handler that treated
// a name as an ID would go on to build a socket path out of it.

// Resolve turns a reference into the ID of the session it names.
//
// An "@id" reference is returned as-is once it is well formed, without checking that the session exists.
// That is deliberate: the caller is about to look it up anyway, and its own error names what it was
// doing, while a check here would report "no such session" from a function the user never invoked.
func (m *Manager) Resolve(ctx context.Context, ref string) (string, error) {
	value, isID := paths.SessionRef(ref)
	if isID {
		if err := paths.ValidateSessionID(value); err != nil {
			return "", err
		}
		return value, nil
	}
	if err := paths.ValidateSessionName(value); err != nil {
		return "", err
	}

	binding, err := m.store.Binding(ctx, value)
	if errors.Is(err, store.ErrNotFound) {
		// Named as a name that resolves to nothing rather than as a missing session, because the two
		// are now different problems with different fixes: the session may well be alive under another
		// name or under its ID, and saying "no session named x" points at that.
		return "", fmt.Errorf("no session is named %q: %w", value, store.ErrNotFound)
	}
	if err != nil {
		return "", err
	}
	return binding.SessionID, nil
}

// Names returns the names bound to a session, oldest first.
func (m *Manager) Names(ctx context.Context, id string) ([]string, error) {
	bindings, err := m.store.BindingsFor(ctx, id)
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(bindings))
	for _, b := range bindings {
		names = append(names, b.Name)
	}
	return names, nil
}

// NamesByID returns every session's names, keyed by ID.
//
// One query for a whole listing rather than one per session, which is what `cm list` needs: a name
// lookup per row turns a listing into N+1 queries, and a listing is the most frequently run command
// there is.
func (m *Manager) NamesByID(ctx context.Context) (map[string][]string, error) {
	bindings, err := m.store.Bindings(ctx)
	if err != nil {
		return nil, err
	}
	out := make(map[string][]string)
	for _, b := range bindings {
		out[b.SessionID] = append(out[b.SessionID], b.Name)
	}
	return out, nil
}

// Label is what to call a session when talking to a person.
//
// Its first name, since that is the one it was created with and the one a person recognizes, or its ID
// with the sigil when nothing names it, so whatever is printed can be typed straight back.
func Label(id string, names []string) string {
	if len(names) > 0 {
		return names[0]
	}
	return paths.FormatSessionID(id)
}

// Bind points a name at a session.
//
// Refuses a name already pointing somewhere else unless the caller says to move it, because the two
// intents are far apart and only one of them is reversible by accident: naming a session is ordinary,
// while quietly taking a name off the session a window is watching is how that window ends up
// somewhere its user did not put it.
func (m *Manager) Bind(
	ctx context.Context, name, id string, onKill store.KillAction, move bool,
) error {
	if err := paths.ValidateSessionName(name); err != nil {
		return err
	}
	if _, err := m.store.Get(ctx, id); err != nil {
		return err
	}

	existing, err := m.store.Binding(ctx, name)
	switch {
	case err == nil && existing.SessionID == id:
		// Already where it is being asked to point. Not an error: binding is how a caller states what it
		// wants, and it already holds.
		return nil
	case err == nil && !move:
		return fmt.Errorf("%q already names %s; pass --move to point it at %s instead",
			name, paths.FormatSessionID(existing.SessionID), paths.FormatSessionID(id))
	case err != nil && !errors.Is(err, store.ErrNotFound):
		return err
	}

	if err := m.store.Bind(ctx, store.Binding{
		Name: name, SessionID: id, OnKill: onKill,
	}); err != nil {
		return err
	}
	m.log.Info("bound name", "name", name, "session", id, "on_kill", onKill)

	// A live session's label follows the name it was most recently given, so later messages about it
	// say what the user just called it.
	if sess, live := m.Get(id); live {
		sess.setLabel(name)
	}
	return nil
}

// Unbind removes a name, leaving the session it pointed at running.
func (m *Manager) Unbind(ctx context.Context, name string) (bool, error) {
	removed, err := m.store.Unbind(ctx, name)
	if err != nil {
		return false, err
	}
	if removed {
		m.log.Info("unbound name", "name", name)
	}
	return removed, nil
}

// releaseNames drops the names of a session being removed for good.
//
// Called where a record is deleted, never where a shell merely exits: an exited session keeps its record
// and its names so that attaching by one revives its content under a new ID. Releasing them at exit
// would make a restored terminal window come back to an empty session, which is the whole thing names
// are there to prevent.
func (m *Manager) releaseNames(ctx context.Context, id string) {
	names, err := m.store.UnbindSession(ctx, id)
	if err != nil {
		// Worth a line rather than an error: the session is gone either way, and a leftover name is
		// recoverable by binding it again, while failing the kill would leave a dead session recorded.
		m.log.Warn("releasing the names of a removed session failed", "session", id, "error", err)
		return
	}
	if len(names) > 0 {
		m.log.Info("released names of a removed session", "session", id, "names", names)
	}
}
