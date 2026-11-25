package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// RemoteBinding is a name on this machine pointing at a session on another host: kitty.1234 pointing at
// white:work once `cm rebind white:work` moved that window there.
//
// The local server keeps the pointer and nothing else. It never dials the host; a client given the name
// reads the pointer and runs against the host itself. See docs/ideas.md, "A window is bound to a name, and
// a name may point to another host".
//
// Always borrowed: killing or closing by the name removes the pointer and leaves the remote session alone,
// as a name `cm rebind` moved locally does.
type RemoteBinding struct {
	Name string
	// Host is the remote's name in [remote.hosts], as a qualified reference spells it.
	Host string
	// Ref is the session on Host as it was given: a name, which recreates its session there once it has
	// ended, or "@id", which never creates.
	Ref       string
	CreatedAt time.Time
}

// Qualified is the reference the pointer stands for, host:ref.
func (b RemoteBinding) Qualified() string { return b.Host + ":" + b.Ref }

// BindRemote points a name at a session on another host, taking the name off any session here.
func (s *Store) BindRemote(ctx context.Context, b RemoteBinding) error {
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("pointing %s at %s: %w", b.Name, b.Qualified(), err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, `DELETE FROM bindings WHERE name = ?`, b.Name); err != nil {
		return fmt.Errorf("pointing %s at %s: %w", b.Name, b.Qualified(), err)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO remote_names (name, host, ref, created_at) VALUES (?, ?, ?, ?)
		ON CONFLICT(name) DO UPDATE SET
			host = excluded.host, ref = excluded.ref, created_at = excluded.created_at`,
		b.Name, b.Host, b.Ref, b.CreatedAt.UnixMilli()); err != nil {
		return fmt.Errorf("pointing %s at %s: %w", b.Name, b.Qualified(), err)
	}
	return tx.Commit()
}

// RemoteBindingFor returns where a name points on another host, or ErrNotFound if it points at none.
func (s *Store) RemoteBindingFor(ctx context.Context, name string) (RemoteBinding, error) {
	b := RemoteBinding{Name: name}
	var created int64
	err := s.db.QueryRowContext(ctx,
		`SELECT host, ref, created_at FROM remote_names WHERE name = ?`, name).Scan(&b.Host, &b.Ref, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return RemoteBinding{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	if err != nil {
		return RemoteBinding{}, fmt.Errorf("reading what %s points at: %w", name, err)
	}
	b.CreatedAt = time.UnixMilli(created)
	return b, nil
}

// RemoteBindings returns every name pointing to another host, by name.
func (s *Store) RemoteBindings(ctx context.Context) ([]RemoteBinding, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT name, host, ref, created_at FROM remote_names ORDER BY name`)
	if err != nil {
		return nil, fmt.Errorf("listing names pointing to other hosts: %w", err)
	}
	defer rows.Close()
	var out []RemoteBinding
	for rows.Next() {
		var (
			b       RemoteBinding
			created int64
		)
		if err := rows.Scan(&b.Name, &b.Host, &b.Ref, &created); err != nil {
			return nil, err
		}
		b.CreatedAt = time.UnixMilli(created)
		out = append(out, b)
	}
	return out, rows.Err()
}

// UnbindRemote removes a pointer and reports whether there was one.
func (s *Store) UnbindRemote(ctx context.Context, name string) (bool, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM remote_names WHERE name = ?`, name)
	if err != nil {
		return false, fmt.Errorf("unbinding %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("unbinding %s: %w", name, err)
	}
	return n > 0, nil
}
