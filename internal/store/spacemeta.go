package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SpaceMeta is one piece of a space's metadata: a value cm stores and never reads, for a tool that keeps
// state beside the sessions, such as a terminal's layout of a space's windows.
//
// On the sessions' host rather than wherever the tool runs, because that is the one place every machine
// viewing the space already reaches. See docs/ideas.md, "Layouts follow the sessions".
type SpaceMeta struct {
	Space string
	Key   string
	// Value is nil in a listing, which carries Size instead, since a listing is for finding keys.
	Value []byte
	Size  int
	// Revision changes on every write and is never reused, so a writer holding one can tell whether
	// anything has been written since it read. Not a count: revisions of one key are not consecutive.
	Revision  int64
	UpdatedAt time.Time
}

// ErrMetaNotFound is returned for a key a space has no value for.
var ErrMetaNotFound = errors.New("no such metadata key")

// RevisionMismatchError is returned when a write was conditional on a revision the key no longer has.
type RevisionMismatchError struct {
	// Current is the key's revision now, 0 when it has none.
	Current int64
}

func (e *RevisionMismatchError) Error() string {
	if e.Current == 0 {
		return "metadata key does not exist"
	}
	return fmt.Sprintf("metadata key is at revision %d", e.Current)
}

// querier is what the metadata lookups need, so they run inside a write's transaction.
type querier interface {
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// requireSpace fails with ErrSpaceNotFound for a space with no record. Reads and removals check, so a
// mistyped space is reported rather than read as one with nothing in it.
func requireSpace(ctx context.Context, db querier, space string) error {
	var n int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM spaces WHERE name = ?`, space).Scan(&n); err != nil {
		return fmt.Errorf("finding space %s: %w", space, err)
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", space, ErrSpaceNotFound)
	}
	return nil
}

// metaRevision is a key's current revision, 0 when it has none.
func metaRevision(ctx context.Context, db querier, space, key string) (int64, error) {
	var rev int64
	err := db.QueryRowContext(ctx,
		`SELECT revision FROM space_meta WHERE space = ? AND key = ?`, space, key).Scan(&rev)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return rev, err
}

// SpaceMetas lists a space's metadata keys starting with prefix, by key, without their values.
func (s *Store) SpaceMetas(ctx context.Context, space, prefix string) ([]SpaceMeta, error) {
	if err := requireSpace(ctx, s.db, space); err != nil {
		return nil, err
	}
	// substr rather than LIKE, which would read '_' in a key as a wildcard.
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, length(value), revision, updated_at FROM space_meta
		 WHERE space = ? AND substr(key, 1, ?) = ? ORDER BY key`,
		space, len(prefix), prefix)
	if err != nil {
		return nil, fmt.Errorf("listing metadata of %s: %w", space, err)
	}
	defer rows.Close()

	var out []SpaceMeta
	for rows.Next() {
		m := SpaceMeta{Space: space}
		var updated int64
		if err := rows.Scan(&m.Key, &m.Size, &m.Revision, &updated); err != nil {
			return nil, err
		}
		m.UpdatedAt = time.UnixMilli(updated)
		out = append(out, m)
	}
	return out, rows.Err()
}

// GetSpaceMeta returns one key's value, or ErrMetaNotFound.
func (s *Store) GetSpaceMeta(ctx context.Context, space, key string) (SpaceMeta, error) {
	if err := requireSpace(ctx, s.db, space); err != nil {
		return SpaceMeta{}, err
	}
	m := SpaceMeta{Space: space, Key: key}
	var updated int64
	err := s.db.QueryRowContext(ctx,
		`SELECT value, revision, updated_at FROM space_meta WHERE space = ? AND key = ?`,
		space, key).Scan(&m.Value, &m.Revision, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return SpaceMeta{}, fmt.Errorf("%s in %q: %w", key, space, ErrMetaNotFound)
	}
	if err != nil {
		return SpaceMeta{}, fmt.Errorf("reading %s in %s: %w", key, space, err)
	}
	m.Size = len(m.Value)
	m.UpdatedAt = time.UnixMilli(updated)
	return m, nil
}

// SetSpaceMeta writes a key's value and returns its new revision. The space is created if it has no record,
// as moving a session into it would.
//
// With ifRevision set the write happens only if the key is at that revision, 0 meaning it must not exist,
// and a *RevisionMismatchError says what it is at instead. Two viewers saving the same layout otherwise
// overwrite each other without either knowing.
func (s *Store) SetSpaceMeta(ctx context.Context, space, key string, value []byte, ifRevision *int64) (int64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("writing %s in %s: %w", key, space, err)
	}
	defer tx.Rollback()

	now := time.Now()
	if err := ensureSpace(ctx, tx, space, now); err != nil {
		return 0, err
	}
	cur, err := metaRevision(ctx, tx, space, key)
	if err != nil {
		return 0, fmt.Errorf("writing %s in %s: %w", key, space, err)
	}
	if ifRevision != nil && *ifRevision != cur {
		return 0, &RevisionMismatchError{Current: cur}
	}
	// A fresh row rather than an update, so the revision is a new AUTOINCREMENT rowid. See the migration.
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM space_meta WHERE space = ? AND key = ?`, space, key); err != nil {
		return 0, fmt.Errorf("writing %s in %s: %w", key, space, err)
	}
	if value == nil {
		// NOT NULL: an empty value is a value, and a nil slice binds as NULL.
		value = []byte{}
	}
	res, err := tx.ExecContext(ctx,
		`INSERT INTO space_meta (space, key, value, updated_at) VALUES (?, ?, ?, ?)`,
		space, key, value, now.UnixMilli())
	if err != nil {
		return 0, fmt.Errorf("writing %s in %s: %w", key, space, err)
	}
	rev, err := res.LastInsertId()
	if err != nil {
		return 0, fmt.Errorf("writing %s in %s: %w", key, space, err)
	}
	return rev, tx.Commit()
}

// DeleteSpaceMeta removes a key, reporting whether it had one. A key that does not exist is not an error,
// since the caller's intent is satisfied, unless ifRevision says it was expected at a revision.
func (s *Store) DeleteSpaceMeta(ctx context.Context, space, key string, ifRevision *int64) (bool, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("removing %s in %s: %w", key, space, err)
	}
	defer tx.Rollback()

	if err := requireSpace(ctx, tx, space); err != nil {
		return false, err
	}
	cur, err := metaRevision(ctx, tx, space, key)
	if err != nil {
		return false, fmt.Errorf("removing %s in %s: %w", key, space, err)
	}
	if ifRevision != nil && *ifRevision != cur {
		return false, &RevisionMismatchError{Current: cur}
	}
	if cur == 0 {
		return false, nil
	}
	if _, err := tx.ExecContext(ctx,
		`DELETE FROM space_meta WHERE space = ? AND key = ?`, space, key); err != nil {
		return false, fmt.Errorf("removing %s in %s: %w", key, space, err)
	}
	return true, tx.Commit()
}
