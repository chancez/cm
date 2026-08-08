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

const sessionColumns = `name, shim_socket, log_path, shim_pid, shell_pid, last_seq,
	state, exit_code, command, cwd, title, rows, cols, owned, created_at, updated_at, env`

// Create inserts a session record.
//
// It fails if the name is taken, including by an exited session, since the record of why a
// session ended is worth keeping until it is explicitly removed.
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
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		sess.Name, sess.ShimSocket, sess.LogPath, sess.ShimPID, sess.ShellPID,
		int64(sess.LastSeq), string(sess.State), sess.ExitCode, sess.Command,
		sess.Cwd, sess.Title, sess.Rows, sess.Cols, sess.Owned,
		sess.CreatedAt.UnixMilli(), sess.UpdatedAt.UnixMilli(), encodeEnv(sess.Env),
	)
	if err != nil {
		return fmt.Errorf("creating session %s: %w", sess.Name, err)
	}
	return nil
}

// Get returns one session by name.
func (s *Store) Get(ctx context.Context, name string) (Session, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT `+sessionColumns+` FROM sessions WHERE name = ?`, name)
	sess, err := scanSession(row)
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return sess, err
}

// List returns sessions whose names start with prefix, oldest first so output is stable.
func (s *Store) List(ctx context.Context, prefix string) ([]Session, error) {
	query := `SELECT ` + sessionColumns + ` FROM sessions`
	var args []any
	if prefix != "" {
		// Escape LIKE wildcards so a name containing % or _ is matched literally.
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(prefix)
		query += ` WHERE name LIKE ? ESCAPE '\'`
		args = append(args, esc+"%")
	}
	query += ` ORDER BY created_at, name`

	rows, err := s.db.QueryContext(ctx, query, args...)
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
func (s *Store) Delete(ctx context.Context, name string) error {
	if _, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE name = ?`, name); err != nil {
		return fmt.Errorf("deleting session %s: %w", name, err)
	}
	return nil
}

// Update applies a set of field changes to a session.
//
// Updates are expressed as a struct of optional fields rather than one method per field
// because the server usually changes several at once, and a single statement keeps the row
// consistent.
type Update struct {
	ShimPID  *int
	ShellPID *int
	LastSeq  *uint64
	State    *State
	ExitCode *int
	Cwd      *string
	Title    *string
	Rows     *int
	Cols     *int
	Owned    *bool
	Env      map[string]string
}

// Apply writes the update, returning ErrNotFound if the session is gone.
func (s *Store) Apply(ctx context.Context, name string, u Update) error {
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
	if u.Owned != nil {
		add("owned", *u.Owned)
	}
	if u.Env != nil {
		add("env", encodeEnv(u.Env))
	}
	if len(sets) == 0 {
		return nil
	}

	add("updated_at", time.Now().UnixMilli())
	args = append(args, name)

	// Column names come from this function's own literals, never from input.
	res, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET `+strings.Join(sets, ", ")+` WHERE name = ?`, args...)
	if err != nil {
		return fmt.Errorf("updating session %s: %w", name, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("updating session %s: %w", name, err)
	}
	if n == 0 {
		return fmt.Errorf("%q: %w", name, ErrNotFound)
	}
	return nil
}

// NextName allocates a unique session name with the given prefix.
//
// The counter only ever increases and names are never reused, even after a session is
// deleted. Reuse would let a client that recorded a name reattach to an unrelated session
// later, which is the hazard the kitty integration's counter file exists to avoid.
//
// The read and write happen in one transaction so two concurrent callers cannot receive
// the same name.
func (s *Store) NextName(ctx context.Context, prefix string) (string, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", fmt.Errorf("allocating name: %w", err)
	}
	defer tx.Rollback()

	var n int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO counters (name, value) VALUES (?, 1)
		 ON CONFLICT(name) DO UPDATE SET value = value + 1
		 RETURNING value`, prefix).Scan(&n)
	if err != nil {
		return "", fmt.Errorf("allocating name for prefix %q: %w", prefix, err)
	}
	if err := tx.Commit(); err != nil {
		return "", fmt.Errorf("allocating name: %w", err)
	}
	return fmt.Sprintf("%s%d", prefix, n), nil
}

// encodeEnv renders captured variables for storage. An empty map is stored as an empty string
// rather than "{}", so a row written before this column existed reads back identically.
func encodeEnv(env map[string]string) string {
	if len(env) == 0 {
		return ""
	}
	b, err := json.Marshal(env)
	if err != nil {
		// Keys and values are strings, so this cannot fail; dropping the value is still better
		// than failing the whole write for something advisory.
		return ""
	}
	return string(b)
}

// decodeEnv parses stored variables, treating anything unreadable as absent.
func decodeEnv(s string) map[string]string {
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
		state     string
		created   int64
		updated   int64
		ownedFlag bool
		envJSON   string
	)
	err := sc.Scan(
		&sess.Name, &sess.ShimSocket, &sess.LogPath, &sess.ShimPID, &sess.ShellPID,
		&lastSeq, &state, &sess.ExitCode, &sess.Command, &sess.Cwd, &sess.Title,
		&sess.Rows, &sess.Cols, &ownedFlag, &created, &updated, &envJSON,
	)
	if err != nil {
		return Session{}, err
	}
	sess.Env = decodeEnv(envJSON)
	sess.LastSeq = uint64(lastSeq)
	sess.State = State(state)
	sess.Owned = ownedFlag
	sess.CreatedAt = time.UnixMilli(created)
	sess.UpdatedAt = time.UnixMilli(updated)
	return sess, nil
}
