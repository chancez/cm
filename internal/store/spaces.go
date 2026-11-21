package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/chancez/cm/internal/spaces"
)

// ErrSpaceNotFound is returned when no space has the name given.
var ErrSpaceNotFound = errors.New("space not found")

// ErrSpaceNotEmpty is returned when removing a space that sessions are still in.
//
// A refusal rather than moving them to default, since a space is how its sessions are found: emptying one
// silently would scatter them into the one space everything else lands in, which is the flat list spaces
// exist to replace.
var ErrSpaceNotEmpty = errors.New("space has sessions")

// ErrDefaultSpace is returned when removing the default space, which always exists so a session created
// without one has somewhere to be.
var ErrDefaultSpace = errors.New("the default space cannot be removed")

// Space is a space's record. Its sessions name it; it holds nothing of theirs.
type Space struct {
	Name      string
	CreatedAt time.Time
}

// execer is what ensureSpace needs, so it runs inside the transaction writing the session that names it.
type execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// ensureSpace creates a space's record if it has none, leaving an existing one as it was.
func ensureSpace(ctx context.Context, db execer, name string, now time.Time) error {
	if _, err := db.ExecContext(ctx,
		`INSERT INTO spaces (name, created_at) VALUES (?, ?) ON CONFLICT(name) DO NOTHING`,
		name, now.UnixMilli()); err != nil {
		return fmt.Errorf("creating space %s: %w", name, err)
	}
	return nil
}

// CreateSpace creates a space, succeeding if it already exists, since the caller's intent is satisfied
// either way.
func (s *Store) CreateSpace(ctx context.Context, name string) error {
	return ensureSpace(ctx, s.db, name, time.Now())
}

// Spaces returns every space, by name.
func (s *Store) Spaces(ctx context.Context) ([]Space, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, created_at FROM spaces ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing spaces: %w", err)
	}
	defer rows.Close()

	var out []Space
	for rows.Next() {
		var (
			sp      Space
			created int64
		)
		if err := rows.Scan(&sp.Name, &created); err != nil {
			return nil, err
		}
		sp.CreatedAt = time.UnixMilli(created)
		out = append(out, sp)
	}
	return out, rows.Err()
}

// DeleteSpace removes an empty space. It refuses the default space, and one any session record is still
// in, ended sessions included: a shelved session is in its space as much as a running one, and is exactly
// what the space is kept for.
func (s *Store) DeleteSpace(ctx context.Context, name string) error {
	if name == spaces.Default {
		return ErrDefaultSpace
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("removing space %s: %w", name, err)
	}
	defer tx.Rollback()

	var n int
	if err := tx.QueryRowContext(ctx,
		`SELECT count(*) FROM sessions WHERE space = ?`, name).Scan(&n); err != nil {
		return fmt.Errorf("removing space %s: %w", name, err)
	}
	if n > 0 {
		return fmt.Errorf("%q: %w (%d)", name, ErrSpaceNotEmpty, n)
	}
	res, err := tx.ExecContext(ctx, `DELETE FROM spaces WHERE name = ?`, name)
	if err != nil {
		return fmt.Errorf("removing space %s: %w", name, err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("removing space %s: %w", name, err)
	} else if n == 0 {
		return fmt.Errorf("%q: %w", name, ErrSpaceNotFound)
	}
	return tx.Commit()
}
