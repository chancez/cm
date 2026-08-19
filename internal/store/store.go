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
	// resume point after a server restart, in the shim's numbering.
	LastSeq uint64
	// ClientSeq is how far the server had served clients, in the numbering clients see.
	//
	// Distinct from LastSeq because output is rewritten on the way through and the rewrite changes
	// length, so the two counts diverge by nine bytes per prompt marker. An adopting server has to
	// start its client log here rather than at LastSeq, or a resuming client asks for a position the
	// new log does not have and loses the bytes in between. See docs/architecture.md.
	ClientSeq uint64
	State     State
	// ExitCode is meaningful only when State is StateExited.
	ExitCode int
	// Command is the argv the session runs, stored for display.
	Command string
	Cwd     string
	Title   string
	Rows    int
	Cols    int
	// PersistRequested records that persistence was asked for, rather than turned on to capture a
	// command's output.
	//
	// The distinction is about lifetime, not storage. `cm run` persists so its output is readable
	// after the command exits, which is what it documents, but such a session is finished business
	// within seconds. Expiry uses this to keep it out of `cm list` for the week a deliberately
	// persisted session is kept.
	PersistRequested bool
	// Env holds the environment variables the most recent client reported, so a shell inside the
	// session can refresh values that describe a terminal which may since have been replaced.
	Env map[string]string
	// Tags are the caller's own key/value labels for this session, used to group and filter it.
	//
	// Persisted, unlike a report: a report describes a running program and would come back after a
	// restart describing one that has since finished, while a tag describes the session itself and
	// has to survive. cm never interprets a key.
	Tags      map[string]string
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

	// Environment variables the latest client reported, as JSON.
	//
	// JSON in a column rather than a side table: this is read and written whole, never queried by
	// key, so a table would add a join for no benefit.
	`
	ALTER TABLE sessions ADD COLUMN env TEXT NOT NULL DEFAULT '';
	`,

	// Whether persistence was asked for, as opposed to enabled to capture a command's output.
	//
	// Needed because the two cases have different lifetimes and a log path alone cannot tell them
	// apart. `cm run` persists so its output is readable after the command exits, which is what its
	// documentation promises, but such a session is finished business within seconds and should not
	// occupy `cm list` for the week a deliberately persisted session gets.
	`
	ALTER TABLE sessions ADD COLUMN persist_requested INTEGER NOT NULL DEFAULT 0;
	`,

	// The caller's own key/value labels, as JSON.
	//
	// JSON in a column again, but not for the reason the env column gives. That one is justified by
	// never being queried by key, and tags *are* queried by key, which is normally the argument for a
	// side table. It is still the wrong shape here: session counts are in the tens, every caller that
	// filters already holds the whole list in memory, and expiry and doctor both list unfiltered
	// anyway. A side table would add a join and a cascade delete to make a linear scan over twenty
	// rows asymptotically better, which at this size it is not. Revisit if sessions ever number in
	// the thousands.
	`
	ALTER TABLE sessions ADD COLUMN tags TEXT NOT NULL DEFAULT '';
	`,

	// Drop the ownership flag, which recorded that a client asked for its session to end when its
	// connection dropped without a detach.
	//
	// Dropped rather than left in place because it was never read back: ownership lived on the
	// attachment, so a session adopted after a server restart had none until its client reattached
	// and said so again, which made the column a write-only record of a request rather than state.
	// Removed with the flag itself; see docs/architecture.md for why the feature went.
	`
	ALTER TABLE sessions DROP COLUMN owned;
	`,

	// How far the server had served *clients*, as distinct from last_seq, which counts the shim's
	// bytes.
	//
	// Two numbering spaces exist because output is rewritten on the way through: RewritePromptRedraw
	// appends ";redraw=0" to a prompt marker that carries no redraw parameter, which is the form real
	// shells send, and that lengthens the chunk by nine bytes. last_seq has to stay in the shim's
	// numbering, since it is what a resubscribe asks the shim for, and the shim knows nothing about
	// the rewrite.
	//
	// Without this column the two were conflated across a restart. A client resumed from a position it
	// had counted in rewritten bytes, while the adopting server started its client log at last_seq, so
	// the client asked for a position ahead of the log and was silently clamped forward past output it
	// never received. Measured at nine bytes per prompt marker, so a session that had run three
	// commands lost 27 bytes: enough to slice the front off an escape sequence, which then renders as
	// literal text. It presented as a corrupted TUI that a forced repaint fixed.
	`
	ALTER TABLE sessions ADD COLUMN client_seq INTEGER NOT NULL DEFAULT 0;
	`,
}
