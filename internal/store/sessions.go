package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const sessionColumns = `id, shim_socket, log_path, shim_pid, shell_pid, last_seq,
	state, exit_code, command, cwd, title, rows, cols, created_at, updated_at, env,
	persist_requested, tags, client_seq`

// Create inserts a session record.
//
// It fails if the ID is taken, which is a bug rather than a condition to handle: IDs come from NewID
// and are never reused, so a conflict means two sessions were built on one identity.
func (s *Store) Create(ctx context.Context, sess Session) error {
	now := time.Now()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	sess.UpdatedAt = now
	if sess.State == "" {
		sess.State = StateRunning
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (`+sessionColumns+`)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.ID, sess.ShimSocket, sess.LogPath, sess.ShimPID, sess.ShellPID,
		int64(sess.LastSeq), string(sess.State), sess.ExitCode, sess.Command,
		sess.Cwd, sess.Title, sess.Rows, sess.Cols,
		sess.CreatedAt.UnixMilli(), sess.UpdatedAt.UnixMilli(), encodeStringMap(sess.Env),
		sess.PersistRequested, encodeStringMap(sess.Tags), int64(sess.ClientSeq),
	)
	if err != nil {
		return fmt.Errorf("creating session %s: %w", sess.ID, err)
	}
	return nil
}

// Get returns one session by ID.
func (s *Store) Get(ctx context.Context, id string) (Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE id = ?`, id)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	return sess, err
}

// List returns every session, oldest first so output is stable between calls.
//
// No prefix filter, unlike the version keyed on names. Filtering by name prefix now means "has a name
// starting with this", which is a question about bindings rather than about sessions, and the caller
// that asks it already holds the whole list: session counts are in the tens, which is the same reason
// tags live in a JSON column rather than a side table. Doing it in Go also deletes the LIKE escaping
// this used to need, and with it the chance of a prefix containing % matching everything.
func (s *Store) List(ctx context.Context) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions ORDER BY created_at, id`

	rows, err := s.db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	defer rows.Close()

	var out []Session
	for rows.Next() {
		sess, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// Delete removes a session record. Removing a session that is already gone is not an
// error, since the caller's intent is satisfied either way.
//
// Names bound to it are not removed here. Deciding what a name means once its session is gone is the
// manager's call, not the store's: an exited session's names have to survive so attaching by one
// revives its content, while a session that has been reaped for good takes its names with it.
func (s *Store) Delete(ctx context.Context, id string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, id); err != nil {
		return fmt.Errorf("deleting session %s: %w", id, err)
	}
	return nil
}

// Update applies a set of field changes to a session.
//
// Updates are expressed as a struct of optional fields rather than one method per field
// because the server usually changes several at once, and a single statement keeps the row
// consistent.
type Update struct {
	ShimPID   *int
	ShellPID  *int
	LastSeq   *uint64
	ClientSeq *uint64
	State     *State
	ExitCode  *int
	Cwd       *string
	Title     *string
	Rows      *int
	Cols      *int
	Env       map[string]string
	// Tags replaces the session's whole tag set rather than merging into it, so removing a tag is
	// expressible. A nil map leaves them alone; an empty but non-nil map clears them, which is what
	// removing the last tag does.
	//
	// Whole-set rather than per-key edits because the caller already has to read the current tags to
	// decide what the new set is, and two callers merging concurrently into the same row would lose
	// one of the edits either way.
	Tags map[string]string
}

// Apply writes the update, returning ErrNotFound if the session is gone.
func (s *Store) Apply(ctx context.Context, id string, u Update) error {
	var (
		sets []string
		args []any
	)
	add := func(col string, val any) {
		sets = append(sets, col+" = ?")
		args = append(args, val)
	}

	if u.ShimPID != nil {
		add("shim_pid", *u.ShimPID)
	}
	if u.ShellPID != nil {
		add("shell_pid", *u.ShellPID)
	}
	if u.ClientSeq != nil {
		add("client_seq", int64(*u.ClientSeq))
	}
	if u.LastSeq != nil {
		add("last_seq", int64(*u.LastSeq))
	}
	if u.State != nil {
		add("state", string(*u.State))
	}
	if u.ExitCode != nil {
		add("exit_code", *u.ExitCode)
	}
	if u.Cwd != nil {
		add("cwd", *u.Cwd)
	}
	if u.Title != nil {
		add("title", *u.Title)
	}
	if u.Rows != nil {
		add("rows", *u.Rows)
	}
	if u.Cols != nil {
		add("cols", *u.Cols)
	}
	if u.Env != nil {
		add("env", encodeStringMap(u.Env))
	}
	if u.Tags != nil {
		add("tags", encodeStringMap(u.Tags))
	}
	if len(sets) == 0 {
		return nil
	}

	add("updated_at", time.Now().UnixMilli())
	args = append(args, id)

	// Column names come from this function's own literals, never from input.
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE id = ?`, args...)
	if err != nil {
		return fmt.Errorf("updating session %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating session %s: %w", id, err)
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	return nil
}

// encodeStringMap renders a map column for storage. An empty map is stored as an empty string
// rather than "{}", so a row written before the column existed reads back identically.
//
// Shared by the env and tags columns, which are both maps read and written whole.
func encodeStringMap(m map[string]string) string {
	if len(m) == 0 {
		return ""
	}
	b, err := json.Marshal(m)
	if err != nil {
		// Keys and values are strings, so this cannot fail; dropping the value is still better
		// than failing the whole write for something advisory.
		return ""
	}
	return string(b)
}

// decodeStringMap parses a stored map column, treating anything unreadable as absent.
func decodeStringMap(s string) map[string]string {
	if s == "" {
		return nil
	}
	var out map[string]string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

// scanner covers both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanSession(sc scanner) (Session, error) {
	var (
		sess      Session
		lastSeq   int64
		clientSeq int64
		state     string
		created   int64
		updated   int64
		envJSON   string
		requested bool
		tagsJSON  string
	)
	err := sc.Scan(
		&sess.ID, &sess.ShimSocket, &sess.LogPath, &sess.ShimPID, &sess.ShellPID,
		&lastSeq, &state, &sess.ExitCode, &sess.Command, &sess.Cwd, &sess.Title,
		&sess.Rows, &sess.Cols, &created, &updated, &envJSON, &requested,
		&tagsJSON, &clientSeq,
	)
	if err != nil {
		return Session{}, err
	}
	sess.Env = decodeStringMap(envJSON)
	sess.Tags = decodeStringMap(tagsJSON)
	sess.LastSeq = uint64(lastSeq)
	sess.ClientSeq = uint64(clientSeq)
	sess.State = State(state)
	sess.PersistRequested = requested
	sess.CreatedAt = time.UnixMilli(created)
	sess.UpdatedAt = time.UnixMilli(updated)
	return sess, nil
}

// SetUpdatedAt overrides a session's last-modified time.
//
// Exists for tests, which need to age a record to exercise expiry without waiting. Kept on the store
// rather than reaching into sqlite from a test, so the column name lives in one place.
func (s *Store) SetUpdatedAt(ctx context.Context, id string, when time.Time) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET updated_at = ? WHERE id = ?`, when.UnixMilli(), id)
	if err != nil {
		return fmt.Errorf("setting updated_at for %s: %w", id, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", id, ErrNotFound)
	}
	return nil
}
