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
	"os"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, so cgo stays confined to internal/vt
)

// ErrNotFound is returned when no session matches.
var ErrNotFound = errors.New("session not found")

// Store holds the sqlite connection.
type Store struct {
	db *sql.DB

	// IDSource overrides how session IDs are drawn, so a test can make them predictable and compare
	// whole records. Nil means the real generator.
	//
	// A field rather than an environment variable on purpose: an env var a released binary honors is
	// one a stale export can use to make it lie, which is what the cm_testhooks build tag exists to
	// prevent. Nothing outside a test can reach this field.
	IDSource func() (string, error)
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
	if err := s.migrate(ctx, path); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// OpenExisting opens the database for reading without migrating it.
//
// For a client, and the distinction is not a nicety: Open applies migrations, so a command that only
// wanted to read a name converted the whole database out from under a running server. Measured against a
// server on the previous build: `cm logs shim <name>` took the schema from 6 to 7 in 0.01s, and every
// request that server served afterwards failed with `SQL logic error: no such column: name`. A snapshot
// was left behind, so it was recoverable, but nothing asked and nothing warned.
//
// The schema has to match this build exactly. Reading an older one is refused rather than attempted
// because the tables a caller wants may not be there yet, and the error for that is a bare "no such
// table" naming a detail the reader cannot act on. Newer is refused for the reason migrate gives.
//
// Not opened with mode=ro, which would look stricter and be worse: a WAL database needs its shared-memory
// file, and a read-only connection cannot create one, so this would fail exactly when no server is running
// and the shm is absent. That is the case this exists to serve, since the reason to read a shim's log is
// usually that something is already wrong. Migration is what had to be prevented, and not calling it is
// what prevents it.
func OpenExisting(ctx context.Context, path string) (*Store, error) {
	// Checked rather than left to sqlite, which creates the file it cannot find. A client that created an
	// empty database would leave one behind for the next server to adopt as the real thing.
	if !fileExists(path) {
		return nil, fmt.Errorf("no database at %s: %w", path, ErrNotFound)
	}

	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	db.SetMaxOpenConns(1)

	s := &Store{db: db}
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		db.Close()
		return nil, fmt.Errorf("reading schema version: %w", err)
	}
	if version != len(migrations) {
		db.Close()
		return nil, fmt.Errorf(
			"%s is at schema version %d and this build knows %d: run a cm command that starts a server to "+
				"migrate it, since a client will not",
			path, version, len(migrations))
	}
	return s, nil
}

// fileExists reports whether a path is there, treating any error as absent.
//
// Only used to decide whether an error message can name a file, so a path that cannot be stat'd is the
// same as one that is not there: the message drops the offer rather than promising something unreadable.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
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
	// ID is the session's identity: allocated when it is created, never reused, never chosen by a
	// caller, and never changed for the life of the session.
	//
	// Names are not identities and are not here. A name is a binding onto an ID, so a session has zero
	// or more of them and any of them can be pointed somewhere else without the session noticing. That
	// split is what makes rename, several names for one session, and moving a window from one session
	// to another all the same operation, and it is why nothing in this package has to move a socket or
	// migrate a row to rename something.
	ID string
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

// BackupPath names the snapshot taken before migrating a database off a given schema version.
//
// Derived from the database path rather than composed in paths, so the two cannot drift: whatever opens the
// database can name its snapshots without being told where they live.
func BackupPath(dbPath string, version int) string {
	return fmt.Sprintf("%s.v%d.bak", dbPath, version)
}

// snapshot copies the database to a standalone file, for rolling back to a build that predates a migration.
//
// VACUUM INTO rather than copying the file. The database runs in WAL mode, so committed rows can still be
// in the -wal file and a copy of cm.db alone can miss them: the snapshot would look fine and be missing the
// most recent sessions. This writes one consistent file with no sidecars.
//
// Called before any migration runs, and deliberately not deleted when one succeeds. A migration that fails
// is already safe, because each runs in a single transaction along with its user_version bump, so sqlite
// rolls the whole thing back; measured against this driver, a failed multi-statement migration left neither
// the new table nor the new version behind. The case that needs a snapshot is a migration that *succeeded*
// and is being rolled back later, which is exactly when deleting it would have thrown away the only copy an
// older build can read.
func (s *Store) snapshot(ctx context.Context, dbPath string, version int) error {
	out := BackupPath(dbPath, version)
	// VACUUM INTO refuses an existing target. An older file for this same version describes the same
	// starting point, so replacing it loses nothing.
	if err := os.Remove(out); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing the previous snapshot %s: %w", out, err)
	}
	if _, err := s.db.ExecContext(ctx, `VACUUM INTO ?`, out); err != nil {
		return fmt.Errorf("snapshotting %s to %s: %w", dbPath, out, err)
	}
	return nil
}

// migrate brings the schema up to date.
//
// Migrations are append-only and tracked by user_version, which sqlite stores in the
// database header. That avoids a separate bookkeeping table and cannot drift from the
// file it describes.
func (s *Store) migrate(ctx context.Context, path string) error {
	var version int
	if err := s.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("reading schema version: %w", err)
	}

	// A database from a newer cm, which happens when a build is rolled back. Refused here, where the
	// cause is knowable, because the alternative is what it did before: migrate had nothing to apply, so
	// Open succeeded and the first query failed against a column the newer schema had removed. That error
	// names a column and says nothing about the binary being older than the file, which is the one fact
	// needed to fix it.
	//
	// Migrations only ever move forward, so there is nothing to undo. The way out is to put the newer
	// build back; failing that, the sessions have to be stopped and the database removed, which is why
	// the message says so rather than suggesting it as a first resort: removing it strands any shim still
	// running, since the record is the only thing that can find one again.
	if version > len(migrations) {
		// The snapshot the newer build took on its way past this version, if it is still there. Naming it
		// is the difference between an explanation and a way out: it is a database this build can read.
		if backup := BackupPath(path, len(migrations)); fileExists(backup) {
			return fmt.Errorf(
				"%s is at schema version %d and this build knows %d: the database was written by a newer "+
					"cm, and schema changes are not reversible. A snapshot from before that migration is "+
					"at %s, which this build can read: stop every session, then move it over %s. Anything "+
					"created since is not in it, and a session missing from it is left running with "+
					"nothing able to find it, so prefer reinstalling the newer cm",
				path, version, len(migrations), backup, path)
		}
		return fmt.Errorf(
			"%s is at schema version %d and this build knows %d: the database was written by a newer cm, "+
				"and schema changes are not reversible. Reinstall the newer build, or stop every session "+
				"and remove the database to start fresh",
			path, version, len(migrations))
	}

	// Snapshot before changing anything, because a schema change cannot be undone: the only way back to a
	// build that predates one is a copy of the database as it was. Skipped at version 0, which is a fresh
	// file with nothing in it worth keeping.
	//
	// A failure here stops the migration rather than proceeding without a snapshot. Carrying on would take
	// the irreversible step precisely when the safety net could not be written, and what causes it -- a full
	// or read-only state directory -- is something the user can fix and would rather be told about. Same
	// reasoning as an unusable persist path failing at creation: silently not doing it is worse than
	// refusing. See docs/persistence.md.
	if version > 0 && version < len(migrations) {
		if err := s.snapshot(ctx, path, version); err != nil {
			return err
		}
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

	// Make the ID the identity and a name nothing but a binding onto one.
	//
	// The previous schema keyed sessions on their name, which made a name an identity: it was the
	// primary key, the variable part of the shim socket path, and the value shells had already exported
	// as CM_SESSION. That is why a session could not be renamed, why it could not have a second name,
	// and why pointing a terminal window at a different session had no expressible form. All three are
	// the same missing indirection, and this migration adds it.
	//
	// A rebuild rather than an ALTER because sqlite cannot change a primary key. Nothing on disk moves:
	// shim_socket and log_path are recorded rather than derived, which is exactly the property they were
	// given so a layout change could not orphan existing sessions, so sessions carried across keep the
	// paths they were created with and only new ones derive theirs from an ID.
	//
	// Backfilled IDs are "mig" plus the old rowid, and cannot collide with a generated one: 'i' is not
	// in the generation alphabet, so no generated ID can begin "mig". They are also recognizable in a
	// log as having predated IDs, which a random string would not be.
	//
	// Every existing name becomes a binding that owns its session, since it was created with it. The
	// bindings table is recreated rather than converted because no command can write one yet, so there
	// is nothing to carry over, and its column is renamed to session_id now that it holds an ID rather
	// than another name.
	//
	// The counters table goes with the name. It existed to allocate implicit names like s17, and an
	// implicit name was only ever a stand-in for an identity, which is what an ID now is.
	`
	CREATE TABLE sessions_by_id (
		id          TEXT PRIMARY KEY,
		shim_socket TEXT NOT NULL,
		log_path    TEXT NOT NULL,
		shim_pid    INTEGER NOT NULL DEFAULT 0,
		shell_pid   INTEGER NOT NULL DEFAULT 0,
		last_seq    INTEGER NOT NULL DEFAULT 0,
		client_seq  INTEGER NOT NULL DEFAULT 0,
		state       TEXT NOT NULL,
		exit_code   INTEGER NOT NULL DEFAULT 0,
		command     TEXT NOT NULL DEFAULT '',
		cwd         TEXT NOT NULL DEFAULT '',
		title       TEXT NOT NULL DEFAULT '',
		rows        INTEGER NOT NULL DEFAULT 0,
		cols        INTEGER NOT NULL DEFAULT 0,
		env         TEXT NOT NULL DEFAULT '',
		persist_requested INTEGER NOT NULL DEFAULT 0,
		tags        TEXT NOT NULL DEFAULT '',
		created_at  INTEGER NOT NULL,
		updated_at  INTEGER NOT NULL
	) STRICT;

	INSERT INTO sessions_by_id (id, shim_socket, log_path, shim_pid, shell_pid, last_seq,
		client_seq, state, exit_code, command, cwd, title, rows, cols, env, persist_requested,
		tags, created_at, updated_at)
	SELECT 'mig' || printf('%05d', rowid), shim_socket, log_path, shim_pid, shell_pid, last_seq,
		client_seq, state, exit_code, command, cwd, title, rows, cols, env, persist_requested,
		tags, created_at, updated_at
	FROM sessions;

	CREATE TABLE names (
		name       TEXT PRIMARY KEY,
		session_id TEXT NOT NULL,
		on_kill    TEXT NOT NULL,
		created_at INTEGER NOT NULL
	) STRICT;

	INSERT INTO names (name, session_id, on_kill, created_at)
	SELECT name, 'mig' || printf('%05d', rowid), 'target', created_at FROM sessions;

	DROP TABLE sessions;
	DROP TABLE counters;
	ALTER TABLE sessions_by_id RENAME TO sessions;
	ALTER TABLE names RENAME TO bindings;

	CREATE INDEX sessions_state ON sessions(state);
	CREATE INDEX bindings_session ON bindings(session_id);
	`,
}
