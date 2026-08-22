package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"time"
)

// KillAction says what killing a session by one of its names does.
//
// Recorded per name rather than passed to the kill, because the caller doing the killing is usually a
// terminal emulator's window-close watcher: it fires for every window and cannot know whether that
// window is where a session lives or borrowed it from somewhere else. Whoever created the name does
// know, so the knowledge is stored where it exists.
//
// This is not the ownership flag that was removed, and the difference is why it is safe. See
// docs/architecture.md: ownership was consulted when a connection dropped, which the server cannot tell
// apart from a terminal quitting, so it destroyed sessions that were meant to be reattached. This is
// only ever consulted on an explicit kill. The caller still decides whether to kill at all; the name
// only says what killing by that name means.
type KillAction string

const (
	// KillUnbind releases the name and leaves the session running, for a name pointed at a session that
	// already existed. Closing a window that borrowed a session is the session equivalent of detaching.
	//
	// The zero value, so a caller that forgets to say ends up with the half that cannot destroy someone
	// else's shell.
	KillUnbind KillAction = "unbind"
	// KillTarget kills the session the name resolves to, for a name that was created along with it.
	// This is what an ordinary `cm kill work` does.
	KillTarget KillAction = "target"
)

// Binding is a name pointing at a session.
//
// The indirection is the point. A session's identity is its ID, so a name can be created, moved to a
// different session, or dropped without the session noticing, and none of it moves a socket or rewrites
// a row. A session may have several names or none at all, and is always reachable by ID either way,
// which is what keeps a shell from being stranded when its last name is pointed elsewhere.
type Binding struct {
	Name      string
	SessionID string
	OnKill    KillAction
	CreatedAt time.Time
}

// Bind points a name at a session, replacing wherever that name pointed before.
//
// An upsert rather than an insert, because moving a name is the ordinary case rather than an error: it
// is how a window is switched to another session, and how a session is renamed. A refused rebind would
// leave a window showing one session while its name said another, which is the state that costs an
// lsof to diagnose.
//
// Nothing here checks that the session exists. A name outliving its session is a state the manager has
// to handle anyway, since an exited session keeps its record so attaching by name can revive its
// content, and a store that refused to bind to a dead ID would make that revival unexpressible.
func (s *Store) Bind(ctx context.Context, b Binding) error {
	if b.OnKill == "" {
		b.OnKill = KillUnbind
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO bindings (name, session_id, on_kill, created_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			session_id = excluded.session_id,
			on_kill = excluded.on_kill,
			created_at = excluded.created_at`,
		b.Name, b.SessionID, string(b.OnKill), b.CreatedAt.UnixMilli())
	if err != nil {
		return fmt.Errorf("binding %s to %s: %w", b.Name, b.SessionID, err)
	}
	return nil
}

// Binding returns what a name points at, or ErrNotFound if nothing does.
func (s *Store) Binding(ctx context.Context, name string) (Binding, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT name, session_id, on_kill, created_at FROM bindings WHERE name = ?`, name)
	b, err := scanBinding(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Binding{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return b, err
}

// Bindings returns every name, oldest first so output is stable between calls.
func (s *Store) Bindings(ctx context.Context) ([]Binding, error) {
	return s.queryBindings(ctx,
		`SELECT name, session_id, on_kill, created_at FROM bindings ORDER BY created_at, name`)
}

// BindingsFor returns the names pointing at one session, oldest first.
//
// Oldest first rather than alphabetical because the first name a session was given is the one a person
// recognizes it by, so it is what a listing should lead with.
func (s *Store) BindingsFor(ctx context.Context, sessionID string) ([]Binding, error) {
	return s.queryBindings(ctx,
		`SELECT name, session_id, on_kill, created_at FROM bindings
		 WHERE session_id = ? ORDER BY created_at, name`, sessionID)
}

func (s *Store) queryBindings(ctx context.Context, query string, args ...any) ([]Binding, error) {
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("listing names: %w", err)
	}
	defer rows.Close()

	var out []Binding
	for rows.Next() {
		b, err := scanBinding(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

// Unbind removes a name and reports whether it was there.
//
// The bool rather than an error for a missing name, because callers need the cases apart in their
// output: unbinding a name nothing used is satisfied already, while reporting "unbound" about a name
// that was never bound is a lie about what happened.
func (s *Store) Unbind(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM bindings WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("unbinding %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unbinding %s: %w", name, err)
	}
	return n > 0, nil
}

// UnbindSession removes every name pointing at a session and returns them.
//
// For a session being removed for good, not one whose shell merely exited: an exited session keeps its
// record and its names, so that attaching by one revives its content under a fresh ID. Calling this at
// exit would make a window come back empty after a reboot, which is the whole thing the names exist to
// prevent.
//
// Returning the names lets the caller log what a removal released, so a window that stops resolving
// where it used to has an explanation in the server log rather than looking like the name was never
// written.
func (s *Store) UnbindSession(ctx context.Context, sessionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`DELETE FROM bindings WHERE session_id = ? RETURNING name`, sessionID)
	if err != nil {
		return nil, fmt.Errorf("releasing the names of %s: %w", sessionID, err)
	}
	defer rows.Close()

	var names []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, fmt.Errorf("releasing the names of %s: %w", sessionID, err)
		}
		names = append(names, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Sorted, because the order a DELETE ... RETURNING produces is sqlite's business and these names
	// are printed. Without this the test that pins the order passes on insertion order happening to
	// match, which is the kind of pass that stops meaning anything the moment a row is rewritten.
	sort.Strings(names)
	return names, nil
}

func scanBinding(sc scanner) (Binding, error) {
	var (
		b       Binding
		onKill  string
		created int64
	)
	if err := sc.Scan(&b.Name, &b.SessionID, &onKill, &created); err != nil {
		return Binding{}, err
	}
	b.OnKill = KillAction(onKill)
	b.CreatedAt = time.UnixMilli(created)
	return b, nil
}
