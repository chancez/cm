// Package store persists session metadata in sqlite.
//
// The database is a durable record, not the authority on whether a session is alive. A
// live session owns a pty and goroutines, none of which can live in a database, so
// liveness is only ever "does the shim answer". When the two disagree, the shim wins.
//
// Terminal output deliberately does not live here. It is high-volume, written
// sequentially, and only ever read back in order, so it belongs in an append-only file
// with the database holding a pointer to it. Putting it in rows would place a SQL insert
// on the hot path.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so cgo stays confined to internal/vt
)

// ErrNotFound is returned when no session matches.
var ErrNotFound = errors.New("session not found")

// Store holds the sqlite connection.
type Store struct {
	db *sql.DB
}

// Open opens or creates the database at path and applies migrations.
func Open(ctx context.Context, path string) (*Store, error) {
	// WAL lets a reader run while a writer commits, which matters because listing
	// sessions should not block a session being created. busy_timeout covers the brief
	// contention that remains; without it a concurrent write fails immediately with
	// SQLITE_BUSY rather than waiting.
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	// modernc's driver serializes access per connection anyway, and a single writer
	// avoids SQLITE_BUSY entirely for our write volume, which is a handful of rows per
	// session lifetime.
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	if err := s.migrate(ctx); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close releases the database.
func (s *Store) Close() error { return s.db.Close() }

// State is a session's lifecycle stage as recorded in the database.
type State string

const (
	// StateRunning means the session was last known to have a live shim.
	StateRunning State = "running"
	// StateExited means the shell exited. The row is kept so `list` can report the exit
	// status and a client can see why its session ended.
	StateExited State = "exited"
	// StateDead means the shim could not be reached when the server last checked. Kept
	// distinct from exited because the outcome is unknown rather than observed, and a
	// future version may be able to resurrect these from the output log.
	StateDead State = "dead"
)

// Session is a persisted session record.
type Session struct {
	Name string
	// ShimSocket is where the server reaches this session's shim. Recorded rather than
	// derived so a socket layout change cannot orphan existing sessions.
	ShimSocket string
	// LogPath is the append-only output log.
	LogPath string
	// ShimPID and ShellPID are recorded for diagnostics and for reaping a shim whose
	// socket has gone but whose process may linger.
	ShimPID  int
	ShellPID int
	// LastSeq is how far the server had consumed the shim's output log. This is the
	// resume point after a server restart.
	LastSeq uint64
	State   State
	// ExitCode is meaningful only when State is StateExited.
	ExitCode int
	// Command is the argv the session runs, stored for display.
	Command string
	Cwd     string
	Title   string
	Rows    int
	Cols    int
	// Owned records that a client claimed this session, so losing that client's
	// connection without an explicit detach should end the session.
	Owned     bool
	CreatedAt time.Time
	UpdatedAt time.Time
}

// migrate brings the schema up to date.
//
// Migrations are append-only and tracked by user_version, which sqlite stores in the
// database header. That avoids a separate bookkeeping table and cannot drift from the
// file it describes.
func (s *Store) migrate(ctx context.Context) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	for i := version; i < len(migrations); i++ {
		tx, err := s.db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("starting migration %d: %w", i+1, err)
		}
		if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("applying migration %d: %w", i+1, err)
		}
		// PRAGMA does not accept a bound parameter, and i is a loop index over a
		// compile-time constant slice, so formatting it is not an injection risk.
		if _, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			tx.Rollback()
			return fmt.Errorf("recording schema version %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("committing migration %d: %w", i+1, err)
		}
	}
	return nil
}

// migrations are applied in order and never edited once released. Changing one would let
// a fresh database and an upgraded one end up with different schemas.
var migrations = []string{
	`
	CREATE TABLE sessions (
		name        TEXT PRIMARY KEY,
		shim_socket TEXT NOT NULL,
		log_path    TEXT NOT NULL,
		shim_pid    INTEGER NOT NULL DEFAULT 0,
		shell_pid   INTEGER NOT NULL DEFAULT 0,
		last_seq    INTEGER NOT NULL DEFAULT 0,
		state       TEXT NOT NULL,
		exit_code   INTEGER NOT NULL DEFAULT 0,
		command     TEXT NOT NULL DEFAULT '',
		cwd         TEXT NOT NULL DEFAULT '',
		title       TEXT NOT NULL DEFAULT '',
		rows        INTEGER NOT NULL DEFAULT 0,
		cols        INTEGER NOT NULL DEFAULT 0,
		owned       INTEGER NOT NULL DEFAULT 0,
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	) STRICT;

	CREATE INDEX sessions_state ON sessions(state);

	-- Session names are allocated from a monotonic counter rather than reusing freed
	-- names. A reused name would let a client reattach to a different session than it
	-- expected, which is exactly the hazard the kitty integration's counter file avoids.
	CREATE TABLE counters (
		name  TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	) STRICT;
	`,
}
