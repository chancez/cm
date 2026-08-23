package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/chancez/cm/internal/cmlog"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seqlog"
	"github.com/chancez/cm/internal/store"
	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// shimReadyTimeout bounds how long to wait for a freshly spawned shim to bind its socket.
const shimReadyTimeout = 10 * time.Second

// implicitPrefix names sessions the server allocates, as opposed to those a user names.
// Keeping them in one namespace makes them easy to list and reap.
const implicitPrefix = "s"

// replayTimeout bounds rebuilding an adopted session's screen.
//
// A bound rather than none: the shim is a local socket delivering bytes it already holds, so this is
// fast, but a wedged shim must not stop the server from starting. Losing one session's scrollback is
// better than never accepting clients.
const replayTimeout = 10 * time.Second

// NewTerminalFunc builds the terminal model for a session.
//
// Injected rather than imported so the server can be built and tested without the cgo
// terminal emulator, and so a nil-returning implementation still yields a working
// multiplexer minus screen restore.
type NewTerminalFunc func(rows, cols uint16) (Terminal, error)

// Manager owns the session registry and the shims behind it.
type Manager struct {
	dirs  paths.Dirs
	store *store.Store
	// selfExe is the binary re-executed to spawn a shim. Resolved once at startup: if the
	// binary is replaced or removed later, a stale path is better than failing to spawn,
	// and it keeps every shim in a server's lifetime consistent.
	selfExe     string
	newTerminal NewTerminalFunc
	// persist decides which sessions survive a reboot and what happens when one is revived. Nil
	// disables persistence entirely, which is the default.
	persist *PersistPolicy
	// log records what the manager does. Never nil, so callers do not have to check.
	log *slog.Logger
	// shimLogRetention is how long an exited shim's diagnostic log is kept. Zero disables pruning.
	//
	// Separate from the PersistPolicy despite both being retention, because this one applies whatever
	// persistence is set to: a shim log is written for every session, and gating it on persist.enabled
	// is the mistake that left a default install with no expiry at all.
	shimLogRetention time.Duration
	// dbBackupRetention is how long a snapshot taken before a schema migration is kept.
	dbBackupRetention time.Duration
	// resizePolicy is applied to every session created or adopted. Empty behaves as ResizeLeader.
	resizePolicy ResizePolicy
	// socketInode is the inode of the server socket as bound at startup, or zero when unknown.
	//
	// Recorded so the server can tell that its own socket path no longer refers to it, which happens when the
	// runtime directory is deleted underneath a running server: it keeps listening on an inode nothing can
	// name, and every later command starts a second server. Comparing the path's inode is the only way to
	// detect that from the inside, since dialing the path reaches whichever server holds it now.
	socketInode uint64
	// version is what this server reports as its build. Held as a field rather than read from paths.Version
	// at each use so a test can set up a mismatch by construction, instead of through an environment
	// variable and a subprocess. Empty means paths.Version, which is what the real server leaves it as.
	version string

	mu sync.Mutex
	// reportedUnreachable records that this server has already logged being unreachable, so the periodic
	// check reports each transition once instead of on every tick.
	reportedUnreachable bool
	sessions            map[string]*Session
}

// PersistPolicy decides which sessions survive a reboot and how they come back.
//
// Passed in rather than read from config here, so the manager and its tests do not depend on the
// config package or on a file existing.
type PersistPolicy struct {
	// Matches reports whether a session name persists by configuration alone.
	Matches func(name string) bool
	// Limits bounds a persisted log.
	Limits seqlog.FileLimits
	// OnRestore is what to do when a dead session with saved content is attached to.
	OnRestore RestoreAction
	// CommandIsSafeToRerun reports whether a recorded command may be re-run without the session
	// having asked. Matches the program name only, so it is a convenience rather than a guarantee.
	CommandIsSafeToRerun func(argv []string) bool
	// ExpireAfter removes a dead persisted session after this long.
	ExpireAfter time.Duration
	// ForgetUnpersistedAfter removes an ended session nobody asked to persist after this long.
	//
	// Separate from ExpireAfter, and much shorter. Such a session is finished business: either it
	// saved nothing, or it saved output only so `cm run` could show it, which is wanted for seconds
	// rather than days. Sharing the persisted lifetime means every short command a user runs stays in
	// `cm list` for a week, which makes the command useless.
	ForgetUnpersistedAfter time.Duration
}

// RestoreAction is what happens when a dead session with saved content is attached to.
type RestoreAction string

const (
	// RestoreShell starts a fresh shell in the recorded directory.
	RestoreShell RestoreAction = "shell"
	// RestoreNone replays the content and starts nothing.
	RestoreNone RestoreAction = "none"
	// RestoreCommand re-runs the recorded command verbatim.
	RestoreCommand RestoreAction = "command"
)

// NewManager creates a manager. Call Reconcile before serving clients.
func NewManager(dirs paths.Dirs, st *store.Store, newTerminal NewTerminalFunc) (*Manager, error) {
	exe, err := os.Executable()
	if err != nil {
		return nil, fmt.Errorf("resolving own path for shim spawning: %w", err)
	}
	return &Manager{
		dirs:        dirs,
		store:       st,
		selfExe:     exe,
		newTerminal: newTerminal,
		sessions:    make(map[string]*Session),
		log:         cmlog.Discard(),
	}, nil
}

// SetResizePolicy sets which client owns a session's size. Applies to sessions created or adopted
// after this call, which in practice means all of them, since it is set before serving.
func (m *Manager) SetResizePolicy(p ResizePolicy) { m.resizePolicy = p }

// SetLogger installs a logger. A discarding logger is used until one is set, so nothing has to
// nil-check.
func (m *Manager) SetLogger(l *slog.Logger) {
	if l != nil {
		m.log = l
	}
}

// SetPersistPolicy enables persistence.
func (m *Manager) SetPersistPolicy(p *PersistPolicy) { m.persist = p }

// SetShimLogRetention sets how long an exited shim's diagnostic log is kept. Zero disables pruning.
func (m *Manager) SetShimLogRetention(d time.Duration) { m.shimLogRetention = d }

// SetDatabaseBackupRetention sets how long a pre-migration snapshot of the database is kept.
func (m *Manager) SetDatabaseBackupRetention(d time.Duration) { m.dbBackupRetention = d }

// ExpireDatabaseBackups removes pre-migration snapshots past their retention, returning how many went.
//
// Swept on a schedule rather than deleted when a migration succeeds, because a snapshot exists for a
// rollback and a rollback happens later or not at all. See store.ExpireBackups for why an age limit is the
// right bound rather than merely a convenient one.
//
// Kept out of ExpireDeadSessions for the same reason PruneShimLogs is: that walks records and deletes rows,
// while this walks files no record ever described.
func (m *Manager) ExpireDatabaseBackups(now time.Time) (int, error) {
	removed, err := store.ExpireBackups(m.dirs.Database(), m.dbBackupRetention, now)
	for _, path := range removed {
		// One line each rather than a count. There are at most a handful, and a snapshot disappearing is
		// the kind of thing someone hunting for a way to roll back needs to find evidence of.
		m.log.Info("removed a pre-migration database snapshot past its retention", "path", path)
	}
	return len(removed), err
}

// SetServerSocketInode records which socket this server is actually serving on.
//
// Called by the command that binds the listener, since only it knows. Left unset by tests and by any caller
// that serves on a socket it opened itself, in which case the check that uses it is skipped rather than
// reporting a false problem.
func (m *Manager) SetServerSocketInode(ino uint64) { m.socketInode = ino }

// SetVersion overrides the build this server reports.
//
// For tests. The behavior worth testing is what the version-skew check does when a client and server
// disagree, and the alternative ways to arrange that are both bad: building two binaries from two git tags is
// a manual procedure rather than a test, and a package-level variable would make the tests order-dependent.
// A field on the Manager is neither.
func (m *Manager) SetVersion(v string) { m.version = v }

// Version reports the build this server identifies itself as.
//
// Falls back to paths.Version rather than storing it at construction, so a Manager built without a version
// still reports the real one and there is no way to accidentally report the empty string.
func (m *Manager) Version() string {
	if m.version != "" {
		return m.version
	}
	return paths.Version()
}

// persistsSession reports whether a session should write its output to disk.
func (m *Manager) persistsSession(name string, requested bool) bool {
	if m.persist == nil {
		return false
	}
	if requested {
		return true
	}
	return m.persist.Matches != nil && m.persist.Matches(name)
}

// Reconcile adopts sessions recorded in the database whose shims are still alive.
//
// This is what makes a server restart survivable: each recorded session is probed, and a
// reachable shim is resubscribed from the sequence number the previous server reached, so
// output produced during the gap is replayed rather than lost.
//
// A session is marked dead only on a definitive connection failure, never on a timeout. A
// busy shim that misses a probe is still holding a live shell, and discarding its record
// would orphan it permanently with no way to reach it again.
func (m *Manager) Reconcile(ctx context.Context) error {
	records, err := m.store.List(ctx, "")
	if err != nil {
		return fmt.Errorf("loading sessions: %w", err)
	}

	for _, rec := range records {
		if rec.State != store.StateRunning {
			continue
		}

		alive, err := probeShim(ctx, rec.ShimSocket)
		if err != nil && !alive {
			// Unreachable in a way that says nothing is listening.
			m.log.Info("session shim is gone, marking dead",
				"session", rec.Name, "socket", rec.ShimSocket, "error", err)
			state := store.StateDead
			if applyErr := m.store.Apply(ctx, rec.Name, store.Update{State: &state}); applyErr != nil {
				return fmt.Errorf("marking %s dead: %w", rec.Name, applyErr)
			}
			continue
		}

		// Resume from where the previous server stopped consuming.
		sess, err := m.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq)
		if err != nil {
			// The shim answered a moment ago, so this is worth reporting but not fatal:
			// the remaining sessions should still come back.
			m.log.Warn("adopting session failed",
				"session", rec.Name, "from_seq", rec.LastSeq, "error", err)
			continue
		}
		m.log.Info("adopted session", "session", rec.Name, "from_seq", rec.LastSeq)
		m.mu.Lock()
		m.sessions[rec.Name] = sess
		m.mu.Unlock()
	}
	return nil
}

// probeShim reports whether a shim answers on its socket.
//
// The returned bool distinguishes "definitely not listening" from "could not tell". Only
// the former justifies declaring a session dead.
func probeShim(ctx context.Context, socket string) (alive bool, err error) {
	d := net.Dialer{Timeout: 2 * time.Second}
	deadline := time.Now().Add(socketRefusalGrace)
	for {
		conn, err := d.DialContext(ctx, "unix", socket)
		if err == nil {
			conn.Close()
			return true, nil
		}

		// A path that does not exist is the one answer worth acting on immediately, and it is the
		// common case for a shim that exited cleanly, since it unlinks its own socket.
		if socketAbsent(err) {
			return false, err
		}
		// A timeout means the shim exists but is slow, which is not something retrying improves.
		// Reported as possibly alive so a busy shim is not discarded.
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return true, err
		}
		// Anything else is a refusal, which a live shim with a full backlog and a stale socket file
		// both produce. Retry: the live one drains its queue and answers, the stale one never will.
		if time.Now().After(deadline) {
			// Out of patience, and nothing has answered in probeRetryWindow. Treated as not
			// listening, which is what lets a genuinely stale socket be cleaned up rather than
			// keeping a dead session alive forever.
			return false, err
		}
		select {
		case <-ctx.Done():
			// Cancelled rather than answered, so say nothing definite. Reported as possibly alive,
			// since the caller's fallback for that is to leave the session alone.
			return true, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// socketAbsent reports whether a dial error proves nothing is serving the path.
//
// Built from measurement on both platforms, because the comment this replaces asserted the opposite
// as fact and was wrong in the destructive direction. The cases, with the errno each actually gives:
//
//	                          darwin        linux
//	missing path              ENOENT        ENOENT        conclusive
//	plain file, not a socket   ENOTSOCK      ECONNREFUSED  differs
//	stale socket, no listener  ECONNREFUSED  ECONNREFUSED  ambiguous
//	live listener, queue full   ECONNREFUSED  EAGAIN        ambiguous
//
// Only a missing path is conclusive on both, so that is all this reports. ENOTSOCK is checked because
// darwin gives it and it is unambiguous there, but it must not be relied on: Linux answers
// ECONNREFUSED for a plain file, indistinguishable from a stale socket, and treating that as absence
// on darwin only would make the two platforms disagree about a destructive decision.
//
// The bottom two rows are the crux. A unix listener refuses connections once its accept queue fills:
// on darwin with the same ECONNREFUSED a dead socket gives, on Linux with EAGAIN. Measured on darwin,
// a listener with a 50ms handler under 64 concurrent dials refused 185160 of 302124 dials while
// accepting throughout; the queue fills at 128 connections there and 4097 on Linux.
//
// So neither a refusal nor EAGAIN is evidence of absence, and this returns false for both. Separating
// them has to be behavioral: a live listener resumes answering once it returns to accepting, measured
// at about 11ms, while a socket whose process is gone refuses for as long as anyone asks. That retry
// belongs to the caller, since only it knows what a wrong answer costs.
func socketAbsent(err error) bool {
	// errors.Is rather than string matching, since a dial error arrives wrapped in *net.OpError.
	// ENOTDIR covers a parent that is gone or is not a directory, which is as conclusive as ENOENT.
	return errors.Is(err, fs.ErrNotExist) || errors.Is(err, syscall.ENOTDIR)
}

// socketRefusalGrace is how long a path must refuse connections without pause before the refusal is
// believed to mean nothing is listening.
//
// Sized from measurement rather than picked: a live listener whose accept queue was deliberately
// filled became dialable again after about 11ms once it resumed accepting, so this is an order of
// magnitude above that. Small enough that a genuinely stale socket does not delay a create
// noticeably, which is the case it trades against, and both directions have a test.
const socketRefusalGrace = 250 * time.Millisecond

// adopt connects to an existing shim and starts consuming from fromSeq.
//
// The terminal model is rebuilt from the shim's retained output first. Without that step a session
// adopted after a server restart has an empty screen: the model lives in the server, so a new server
// starts with a blank one, and consuming from fromSeq only ever sees output produced from now on. The
// shim still holds the earlier bytes, so the scrollback is recoverable rather than gone.
//
// fromSeq is in the shim's numbering, since it is what the resubscribe and the history replay both ask
// the shim for. clientSeq is where the new session's client log begins, in the numbering clients see,
// and the two are not interchangeable: the prompt rewrite lengthens output, so they diverge by nine
// bytes per prompt marker. Passing fromSeq for both is what made a resuming client ask for a position
// its new server's log did not have, and the bytes in between were dropped without a word. Zero means
// "same as fromSeq", which is right for a brand new session, where nothing has been rewritten yet and
// both spaces start together.
func (m *Manager) adopt(
	ctx context.Context, rec store.Session, fromSeq, clientSeq uint64,
) (*Session, error) {
	term, err := m.buildTerminal(uint16(rec.Rows), uint16(rec.Cols))
	if err != nil {
		return nil, err
	}
	if term != nil {
		if err := m.replayShimHistory(ctx, rec, fromSeq, term); err != nil {
			// A screen that could not be rebuilt is worth reporting, but the session works without
			// it: only restore and history are affected, which is where this started.
			m.log.Warn("rebuilding the screen for an adopted session failed",
				"session", rec.Name, "error", err)
		}
	}
	// A record written before client_seq existed has zero there, which is indistinguishable from a
	// session that genuinely served nothing. Falling back to fromSeq is what an upgrade wants: it
	// reproduces the old behavior for that one adoption rather than starting the log at zero, which
	// would make every subscriber look like it had missed the entire session.
	if clientSeq == 0 {
		clientSeq = fromSeq
	}
	sess, err := newSession(rec, term, fromSeq, clientSeq)
	if err != nil {
		if term != nil {
			term.Close()
		}
		return nil, err
	}
	sess.log = m.log.With("session", rec.Name)
	sess.SetResizePolicy(m.resizePolicy)

	// Persist what the shell reports about itself, so `list` and a terminal emulator opening a
	// new window see current values rather than whatever was true at creation.
	go m.persistMetadata(sess)

	go m.watch(sess)
	return sess, nil
}

// replayShimHistory feeds a shim's retained output into a terminal model, up to fromSeq.
//
// Stops at fromSeq because that is where the session's own pump takes over. Replaying past it would
// write the same bytes twice, and a terminal fed duplicate output shows duplicated lines.
//
// Writes only to the model, never to the session's client log: these bytes are history that clients
// either already saw or will receive as part of a restored screen. Appending them would replay old
// output to an attached client as though it were new.
func (m *Manager) replayShimHistory(
	ctx context.Context, rec store.Session, fromSeq uint64, term Terminal,
) error {
	conn, shim, err := dialShim(rec.ShimSocket)
	if err != nil {
		return err
	}
	defer conn.Close()

	st, err := shim.State(ctx, &shimv1.StateRequest{})
	if err != nil {
		return fmt.Errorf("reading shim state: %w", err)
	}
	if st.OldestSeq >= fromSeq {
		// Nothing retained before where the pump will start, so there is no history to replay.
		return nil
	}

	// Bounded, so a shim that keeps producing cannot make this loop run forever: only the bytes that
	// existed before the pump's starting point are wanted.
	replayCtx, cancel := context.WithTimeout(ctx, replayTimeout)
	defer cancel()

	sub, err := shim.Subscribe(replayCtx, &shimv1.SubscribeRequest{FromSeq: st.OldestSeq})
	if err != nil {
		return fmt.Errorf("subscribing for history: %w", err)
	}

	for {
		out, err := sub.Recv()
		if err != nil {
			// Reaching the end of what is retained is the normal exit, not a failure: whatever was
			// written before this point is what the screen is rebuilt from.
			break
		}
		data := out.Data
		// Trim the tail that the pump will deliver, so the boundary byte is written exactly once.
		if end := out.Seq + uint64(len(data)); end > fromSeq {
			if out.Seq >= fromSeq {
				break
			}
			data = data[:fromSeq-out.Seq]
		}
		if err := term.Write(data); err != nil {
			return fmt.Errorf("replaying output: %w", err)
		}
		if out.Seq+uint64(len(data)) >= fromSeq {
			break
		}
	}

	// Discard anything the emulator generated in response: those answer queries from a program that
	// asked before this server existed, and nothing is waiting for the replies.
	term.TakePending()
	return nil
}

func (m *Manager) buildTerminal(rows, cols uint16) (Terminal, error) {
	if m.newTerminal == nil {
		return nil, nil
	}
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	return m.newTerminal(rows, cols)
}

// persistMetadata writes title and directory changes to the store until the session ends.
func (m *Manager) persistMetadata(sess *Session) {
	sub := sess.subscribeMetadata()
	defer sess.unsubscribeMetadata(sub)

	for {
		select {
		case meta := <-sub.ch:
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			upd := store.Update{Title: &meta.Title}
			// Only record a directory that exists on this machine. A session that has ssh'd
			// elsewhere reports a remote path, and storing it would send a new window somewhere
			// that does not exist locally.
			if meta.Cwd.IsLocal && meta.Cwd.Path != "" {
				upd.Cwd = &meta.Cwd.Path
			}
			if err := m.store.Apply(ctx, sess.name, upd); err != nil {
				// Advisory, so the session continues. Logged because a `list` showing a stale
				// directory is otherwise unexplainable.
				m.log.Warn("recording session metadata failed",
					"session", sess.name, "error", err)
			}
			cancel()
		case <-sess.Done():
			return
		}
	}
}

// watch records a session's outcome once it ends and drops it from the registry.
func (m *Manager) watch(sess *Session) {
	<-sess.Done()

	// A session this server deliberately let go of is still alive; its record must stay
	// as-is so the next server adopts it.
	if sess.Releasing() {
		m.mu.Lock()
		if m.sessions[sess.name] == sess {
			delete(m.sessions, sess.name)
		}
		m.mu.Unlock()
		return
	}

	_, code := sess.Ended()

	// Deliberately no "has this been replaced" check here.
	//
	// One was added and removed: guarding the write on `m.sessions[name] == sess` looked like protection
	// against an older incarnation clobbering a newer row, and it silently broke every detached session.
	// A session whose last client has gone is removed from the registry, so `cm run -d` reaches here with
	// no entry under its name, the check called that "replaced", and the outcome was never written --
	// leaving `cm status` reporting a finished session as running while `cm list` showed exited. The
	// registry answers "is this session being proxied now", not "is this the current session for this
	// name", and only the delete below may use it.
	//
	// The clobbering it was meant to prevent was never the real cause of what prompted it either: that was
	// a replacement shim unable to claim the socket while the old one still held it. That incident is why
	// the upgradable-shims entry in docs/ideas.md rules out fd-passing between two processes and re-execs
	// a single one instead.

	state := store.StateExited
	if code < 0 {
		// The shim vanished rather than the shell exiting, so the outcome is unknown.
		state = store.StateDead
		code = 0
	}

	// An interactive session whose shell exited is finished business, so its record goes now rather than
	// waiting out the forget interval.
	//
	// The distinction is between a session a person was sitting in and one whose result nobody has
	// collected yet. Typing `exit` in a window means the output was already on screen and the person left;
	// there is no status left to report and nothing anyone asked to keep, so a record that lingers is a
	// placeholder holding an exit code nobody will read. Under the previous behavior every closed window
	// left one in `cm list` for five minutes, and with one session per window they accumulated visibly.
	//
	// zmx draws the same line, which is what prompted this: `zmx a foo` then `exit` removes the session at
	// once, while a detached `zmx run -d` that finishes keeps its record with `ended=` and `exit_code=`.
	// Measured both ways rather than assumed.
	//
	// The two kinds are told apart by whether anything ever attached and whether the output was being
	// saved. A `cm run` task sets CaptureOutput so its output outlives it, and a caller reads its status
	// from the record afterwards, so those are kept for the forget interval as before. Same for a session
	// asked to persist, and for one that ended without a client ever attaching, where the record is the
	// only evidence it ran at all.
	//
	// EverWatched rather than the current client count, and rather than any attachment: a client detaching
	// as its shell exits is exactly the shape of typing `exit`, so the count is already zero by the time
	// this runs, and `cm run` and `--no-attach` both attach briefly just to create the session.
	if state == store.StateExited && sess.EverWatched() &&
		!sess.record.PersistRequested && sess.record.LogPath == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := m.store.Delete(ctx, sess.name); err != nil {
			// Not fatal: the record simply expires on the usual schedule instead, which is the behavior
			// this replaces.
			m.log.Warn("forgetting an exited interactive session failed",
				"session", sess.name, "error", err)
		} else {
			m.log.Info("forgot exited interactive session",
				"session", sess.name, "exit_code", code)
		}

		m.mu.Lock()
		if m.sessions[sess.name] == sess {
			delete(m.sessions, sess.name)
		}
		m.mu.Unlock()
		return
	}

	seq, clientSeq := sess.resumePoints()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Apply(ctx, sess.name, store.Update{
		State:     &state,
		ExitCode:  &code,
		LastSeq:   &seq,
		ClientSeq: &clientSeq,
	}); err != nil {
		// A record that is already gone is the expected outcome of a kill, not a failure.
		//
		// Kill deletes the row and then this runs when the pump notices the shell exited, so the two
		// race by design and the delete usually wins. Logged as an error, it made `cm doctor` report an
		// error on every healthy `cm kill` -- which is worse than saying nothing, because a diagnostic
		// that cries wolf teaches people to ignore it.
		//
		// Anything else still matters: without the write, the next server sees a session as running and
		// tries to adopt a shim that is gone.
		if errors.Is(err, store.ErrNotFound) {
			m.log.Debug("session record was already removed when its outcome was recorded",
				"session", sess.name, "state", state)
		} else {
			m.log.Error("recording session outcome failed",
				"session", sess.name, "state", state, "error", err)
		}
	}
	m.log.Info("session ended", "session", sess.name, "state", state, "exit_code", code)

	m.mu.Lock()
	if m.sessions[sess.name] == sess {
		delete(m.sessions, sess.name)
	}
	m.mu.Unlock()
}

// OpenOptions describes an attach request that may need to create the session.
type OpenOptions struct {
	// Name of the session. Empty asks the server to allocate one.
	Name string
	// Rows and Cols size a newly created session.
	Rows, Cols uint16
	// XPixel and YPixel size a newly created session in pixels, zero when the client did not report
	// them. Passed to the shim so a program that reads the pty's pixel dimensions before doing anything
	// else, such as `kitten icat`, sees them in a session that has never been resized.
	XPixel, YPixel uint16
	// Command overrides the user's shell for a new session.
	Command []string
	// Dir is the working directory for a new session.
	Dir string
	// Env holds extra KEY=VALUE entries for a new session.
	Env []string
	// Persist requests that this session's content survive a reboot, regardless of whether its
	// name matches the configured patterns.
	Persist bool
	// CaptureOutput saves this session's output so it can be read after the session ends, without
	// asking for reboot survival.
	//
	// Separate from Persist because the two differ in lifetime rather than mechanism. `cm run` sets
	// this so `cm history` works once the command exits, which is what it documents, but such a
	// session is finished business in seconds and must not sit in `cm list` for the week a
	// deliberately persisted session is kept.
	CaptureOutput bool
	// OnRestore overrides the configured restore behavior for this session. Empty means the
	// configured default.
	OnRestore RestoreAction
	// Tags are the caller's own labels for a newly created session. Ignored when the session
	// already exists, since an attach is not how tags are changed.
	Tags map[string]string

	// restoreFrom is a saved log to replay before the session starts producing output. Set
	// internally when reviving a dead session, never by a caller.
	restoreFrom string
	// ClientEnv holds terminal-related variables from the attaching client, recorded so a shell
	// in the session can refresh them later.
	ClientEnv map[string]string
}

// Open returns the named session, creating it if necessary, and reports whether it created
// it.
func (m *Manager) Open(ctx context.Context, opts OpenOptions) (*Session, bool, error) {
	if opts.Name == "" {
		name, err := m.store.NextName(ctx, implicitPrefix)
		if err != nil {
			return nil, false, err
		}
		opts.Name = name
	}
	if err := paths.ValidateSessionName(opts.Name); err != nil {
		return nil, false, err
	}

	m.mu.Lock()
	if sess, ok := m.sessions[opts.Name]; ok {
		m.mu.Unlock()
		return sess, false, nil
	}
	m.mu.Unlock()

	// A record can exist without a live session: the shell exited, a previous server marked it
	// dead, or the machine rebooted. The shell cannot be resumed either way, but its content can,
	// so the record is examined before being replaced.
	if rec, err := m.store.Get(ctx, opts.Name); err == nil {
		if rec.State == store.StateRunning {
			// Recorded as running but not in our registry, which happens if Reconcile
			// could not adopt it. Try once more before giving up.
			if alive, _ := probeShim(ctx, rec.ShimSocket); alive {
				sess, err := m.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq)
				if err == nil {
					m.mu.Lock()
					m.sessions[opts.Name] = sess
					m.mu.Unlock()
					return sess, false, nil
				}
			}
		}

		// Carry forward what a restore needs, since the record is about to be deleted.
		opts = m.inheritForRestore(opts, rec)

		// Say that the old incarnation is being discarded, and what was lost with it.
		//
		// This is destructive and was silent: attaching to a name whose shell had exited started a fresh
		// shell under it, and the exit status and pid that were the only evidence the previous run
		// happened went with the deleted row. A caller reattaching to what it thought was its session got
		// a new one and no indication of the substitution.
		//
		// Logged rather than refused, because refusing would break the case this path exists for: a
		// terminal emulator restoring a saved window attaches by name and must get a working session
		// whether or not the previous one is still alive. What it should not do is pretend nothing was
		// there.
		if rec.State != store.StateRunning {
			restoring := opts.restoreFrom != ""
			m.log.Info("replacing an ended session with a new shell under the same name",
				"session", opts.Name, "previous_state", rec.State,
				"previous_exit_code", rec.ExitCode, "previous_shell_pid", rec.ShellPID,
				"content_restored", restoring)
		}

		if err := m.store.Delete(ctx, opts.Name); err != nil {
			return nil, false, fmt.Errorf("replacing stale record for %s: %w", opts.Name, err)
		}
	} else if !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}

	sess, err := m.create(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	return sess, true, nil
}

// inheritForRestore carries a dead session's saved state into the options that will recreate it.
//
// The record is deleted immediately after, so anything a restore needs has to be read here.
// Deliberately does not override what the caller asked for: an explicit directory or command on the
// new attach wins, because the user asking for something specific outranks what a previous
// incarnation happened to be doing.
func (m *Manager) inheritForRestore(opts OpenOptions, rec store.Session) OpenOptions {
	// Tags first, and deliberately not gated on persistence below. The record is about to be deleted
	// whether or not it had a log, so a session whose shell exited and is attached to again would
	// otherwise silently lose its tags, and it would lose them on a plain install where persistence
	// is off entirely. Tags describe the session rather than its content, so they survive on their
	// own terms.
	//
	// Merged with the caller's rather than replaced by them, with the caller winning per key.
	// Replacing wholesale would mean that retagging one thing on reattach dropped every other tag,
	// which is not what asking for one tag says.
	if len(rec.Tags) > 0 {
		merged := make(map[string]string, len(rec.Tags)+len(opts.Tags))
		for k, v := range rec.Tags {
			merged[k] = v
		}
		for k, v := range opts.Tags {
			merged[k] = v
		}
		opts.Tags = merged
	}

	if m.persist == nil || rec.LogPath == "" {
		return opts
	}

	// The replay path needs the log, and it only exists for a session that was persisting.
	opts.restoreFrom = rec.LogPath
	opts.Persist = true

	if opts.Dir == "" && rec.Cwd != "" {
		opts.Dir = rec.Cwd
	}

	action := opts.OnRestore
	if action == "" {
		action = m.persist.OnRestore
	}
	if action == "" {
		action = RestoreShell
	}

	// Re-running the recorded command is the one behavior that executes something the user did not
	// type just now, so it happens only when asked for explicitly or when the program is on the
	// allowlist.
	if action == RestoreCommand && len(opts.Command) == 0 && rec.Command != "" {
		argv := strings.Fields(rec.Command)
		allowed := opts.OnRestore == RestoreCommand
		if !allowed && m.persist.CommandIsSafeToRerun != nil {
			allowed = m.persist.CommandIsSafeToRerun(argv)
		}
		if allowed {
			opts.Command = argv
		}
	}
	if action == RestoreNone {
		// Nothing is started. The client still gets the replayed screen, so the session reads as
		// history rather than as something running.
		opts.Command = []string{holdCommand}
	}

	return opts
}

// holdCommand is what a session runs when its restore behavior is "none".
//
// A process that waits rather than no process at all, because the rest of cm assumes a session has
// a pty and a child: without one there is nothing to attach to, resize, or read from. Reading from
// an empty stdin blocks until the session is killed, which is the desired "nothing is running"
// behavior with none of the special cases.
const holdCommand = "cat"

// create spawns a shim and records the session.
func (m *Manager) create(ctx context.Context, opts OpenOptions) (*Session, error) {
	if opts.Rows == 0 || opts.Cols == 0 {
		opts.Rows, opts.Cols = 24, 80
	}

	socket := m.dirs.ShimSocket(opts.Name)
	if err := paths.CheckSocketPath(socket); err != nil {
		return nil, err
	}

	// Wait for a previous shim on this socket to go before spawning one that has to bind it.
	//
	// A shim stays reachable briefly after its shell exits, deliberately: it is the only thing that knows
	// the exit status, so it lingers for exitGrace to be asked. Recreating a session under the same name
	// inside that window meant the new shim found the socket taken and died with "already served by a live
	// shim", while waitForShim below dialed the *old* shim, got an answer, and reported the new one ready.
	// The record was then left reading the previous incarnation's exit status with shell_pid 0, and nothing
	// was running.
	//
	// Reached by attaching to a session whose shell has exited, which is what a terminal emulator does when
	// it restores a saved window. Measured with `cm run --session r -- sh -c 'exit 7'` followed by
	// `cm attach --no-attach r`.
	//
	// Waiting here rather than loosening the shim's socket check, which refuses to unlink a socket that
	// something answers on for a good reason: removing a live shim's socket orphans it with no way back.
	if err := waitForSocketFree(ctx, socket, shimReleaseTimeout); err != nil {
		return nil, fmt.Errorf("waiting for the previous shim for %s to exit: %w", opts.Name, err)
	}

	// Either reason produces a log; only the first makes the session long-lived.
	requested := m.persistsSession(opts.Name, opts.Persist)
	persisting := requested || opts.CaptureOutput

	logPath := ""
	if persisting {
		logPath = m.dirs.SessionLog(opts.Name)
	}

	rec := store.Session{
		Name:       opts.Name,
		ShimSocket: socket,
		LogPath:    logPath,
		State:      store.StateRunning,
		Command:    strings.Join(opts.Command, " "),
		Cwd:        opts.Dir,
		Rows:       int(opts.Rows),
		Cols:       int(opts.Cols),
		Env:        opts.ClientEnv,
		Tags:       opts.Tags,
		// Records why there is a log, which is what expiry keys off.
		PersistRequested: requested,
	}
	// Record before spawning so a shim can never exist without a row describing it. A row
	// with no shim is recoverable; a shim with no row is invisible.
	if err := m.store.Create(ctx, rec); err != nil {
		return nil, err
	}

	pid, err := m.spawnShim(ctx, opts, socket, logPath)
	if err != nil {
		_ = m.store.Delete(ctx, opts.Name)
		return nil, err
	}
	rec.ShimPID = pid
	if err := m.store.Apply(ctx, opts.Name, store.Update{ShimPID: &pid}); err != nil {
		return nil, err
	}

	sess, err := m.adopt(ctx, rec, 0, 0)
	if err != nil {
		_ = m.store.Delete(ctx, opts.Name)
		return nil, err
	}

	// Replay a previous incarnation's screen, so the first client sees what was there before the
	// reboot rather than a bare prompt.
	//
	// A failed replay is not fatal: the session works, it simply starts empty, which is strictly
	// better than refusing to open it because a cache could not be read.
	if opts.restoreFrom != "" {
		limits := seqlog.FileLimits{}
		if m.persist != nil {
			limits = m.persist.Limits
		}
		blob, _, rerr := replayPersisted(
			opts.restoreFrom, m.newTerminal, opts.Rows, opts.Cols, limits,
		)
		switch {
		case rerr != nil:
			// Not fatal, but the user asked for their content back and did not get it, so this is
			// exactly the kind of silent degradation the log exists for.
			m.log.Warn("replaying persisted session failed",
				"session", opts.Name, "path", opts.restoreFrom, "error", rerr)
		case len(blob) > 0:
			sess.setRestored(blob)
			m.log.Info("restored session from disk",
				"session", opts.Name, "restore_bytes", len(blob))
		}
	}

	// Record the shell's pid for reporting, best effort: the session works without it.
	if st, err := sess.State(ctx); err == nil {
		shellPID := int(st.ShellPid)
		if err := m.store.Apply(ctx, opts.Name, store.Update{ShellPID: &shellPID}); err != nil {
			m.log.Warn("recording shell pid failed", "session", opts.Name, "error", err)
		}
	} else {
		m.log.Warn("reading shim state failed", "session", opts.Name, "error", err)
	}

	m.log.Info("created session",
		"session", opts.Name, "shim_pid", pid, "persisting", persisting,
		"rows", opts.Rows, "cols", opts.Cols)

	m.mu.Lock()
	m.sessions[opts.Name] = sess
	m.mu.Unlock()
	return sess, nil
}

// shimArgs builds the argv a shim is spawned with.
//
// Separated from spawnShim so what is passed can be asserted without spawning a process. That matters
// because the failure mode here is silent: a field that OpenOptions carries and this function forgets
// produces a working session with one thing missing, which is how a client's pixel size reached the
// server and never reached the pty. `kitten icat` then refused to draw anything, and the error it
// printed named the terminal rather than cm.
func (m *Manager) shimArgs(opts OpenOptions, logPath string) []string {
	args := []string{
		"--runtime-dir", m.dirs.Runtime,
		"--state-dir", m.dirs.State,
		"shim",
		"--session", opts.Name,
		"--rows", strconv.Itoa(int(opts.Rows)),
		"--cols", strconv.Itoa(int(opts.Cols)),
	}
	// Only when known. A client that reported no pixel size must leave the pty's fields zero, since
	// that is how a program tells that the terminal cannot report them.
	if opts.XPixel > 0 && opts.YPixel > 0 {
		args = append(args,
			"--xpixel", strconv.Itoa(int(opts.XPixel)),
			"--ypixel", strconv.Itoa(int(opts.YPixel)),
		)
	}
	if logPath != "" {
		args = append(args, "--persist-path", logPath)
		if m.persist != nil {
			if m.persist.Limits.MaxLines > 0 {
				args = append(args, "--persist-max-lines", strconv.Itoa(m.persist.Limits.MaxLines))
			}
			if m.persist.Limits.MaxBytes > 0 {
				args = append(args, "--persist-max-bytes",
					strconv.FormatInt(m.persist.Limits.MaxBytes, 10))
			}
		}
	}
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	if len(opts.Command) > 0 {
		args = append(args, "--")
		args = append(args, opts.Command...)
	}
	return args
}

// spawnShim re-execs this binary as a shim and waits for its socket.
//
// Go cannot fork, so re-exec replaces the double-fork a C implementation would use. The
// child is deliberately not waited on: it must outlive this server, so it is reparented to
// init by letting this process release it. Setsid detaches it from the server's session so
// a signal sent to the server's process group cannot reach it.
func (m *Manager) spawnShim(ctx context.Context, opts OpenOptions, socket, logPath string) (int, error) {
	args := m.shimArgs(opts, logPath)

	cmd := exec.Command(m.selfExe, args...)
	cmd.Env = append(os.Environ(), opts.Env...)
	cmd.SysProcAttr = newShimSysProcAttr()
	// The shim's own stdio is not a terminal and must not be the server's: inheriting it
	// would tie the shim's lifetime to the server's console.
	cmd.Stdin, cmd.Stdout, cmd.Stderr = nil, nil, nil

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("spawning shim for %s: %w", opts.Name, err)
	}
	pid := cmd.Process.Pid

	// Release the child rather than waiting: it outlives this server by design. Without
	// this the process would remain a zombie until the server exits.
	go func() { _ = cmd.Wait() }()

	if err := waitForShim(ctx, socket); err != nil {
		return pid, fmt.Errorf("shim for %s did not become ready: %w", opts.Name, err)
	}
	return pid, nil
}

// waitForShim polls until the shim's socket accepts a connection.
//
// Polling rather than a readiness handshake keeps the shim simpler, and the socket
// appearing is exactly the condition that matters: the shim binds before spawning the
// shell, so a connectable socket means the session is usable.
func waitForShim(ctx context.Context, socket string) error {
	deadline := time.Now().Add(shimReadyTimeout)
	for {
		if conn, err := net.Dial("unix", socket); err == nil {
			conn.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out after %s", shimReadyTimeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// shimReleaseTimeout bounds how long to wait for a previous shim to release its socket.
//
// Longer than the shim's own exitGrace, which is how long it stays reachable after its shell exits: this
// has to outlast that or it would give up while the old shim was still doing what it is supposed to.
const shimReleaseTimeout = 5 * time.Second

// waitForSocketFree blocks until nothing answers on a socket path.
//
// The inverse of waitForShim, and needed for the same reason that one exists: a socket path is the only
// name a shim has, so two incarnations of a session cannot overlap on it. Dialing rather than checking
// whether the file exists, because a stale file nothing answers on is exactly the case that must not wait
// -- the shim removes such a file itself when it binds.
func waitForSocketFree(ctx context.Context, socket string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	// Tracks how long the path has been refusing connections without interruption. A stale socket
	// refuses forever, a busy listener recovers within a few milliseconds, and the errno is identical,
	// so elapsed refusal is the only thing that separates them.
	refusingSince := time.Time{}

	for {
		conn, err := net.Dial("unix", socket)
		switch {
		case err == nil:
			conn.Close()
			refusingSince = time.Time{}

		case socketAbsent(err):
			// Conclusive: nothing can be listening on a path that does not exist or is not a socket.
			return nil

		default:
			// A refusal, which a live shim with a full accept queue and a socket left behind by a
			// crashed one both produce. Returning here is what the bug was: it let a replacement shim
			// try to bind a path a live shim still held, which fails with an error naming the socket
			// rather than the cause. Surfaced as TestWaitForSocketFreeWaitsForALiveListenerToClose
			// failing under a parallel `go test ./...`.
			//
			// Waiting forever is not right either, since a stale socket must not stall every create
			// for the whole timeout. So a sustained refusal is taken as absence.
			if refusingSince.IsZero() {
				refusingSince = time.Now()
			}
			if time.Since(refusingSince) >= socketRefusalGrace {
				return nil
			}
		}

		if time.Now().After(deadline) {
			// Reported rather than spawning anyway, because spawning would fail with a socket error that
			// says nothing about the cause. A shim that will not exit is a real problem and this names it.
			return fmt.Errorf("still answering after %s", timeout)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Get returns a live session by name.
func (m *Manager) Get(name string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[name]
	return sess, ok
}

// List returns session records, with live details filled in from the registry.
func (m *Manager) List(ctx context.Context, prefix string) ([]store.Session, error) {
	records, err := m.store.List(ctx, prefix)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range records {
		if sess, ok := m.sessions[records[i].Name]; ok {
			records[i].LastSeq, records[i].ClientSeq = sess.resumePoints()
		}
	}
	return records, nil
}

// SetTags changes a session's tags, returning the resulting set.
//
// The read and the write are not in one transaction, so two callers editing the same session at the
// same time can lose one edit. Accepted rather than solved: tags are edited by hand or by a script
// that knows what it wants, the store serializes writes to a single connection anyway, and a
// compare-and-swap would push a version number into an API where nothing else needs one. What is
// avoided is worse: applying set and remove as separate writes, which would leave a session
// half-retagged if the second failed.
func (m *Manager) SetTags(
	ctx context.Context, name string, set map[string]string, remove []string, replace bool,
) (map[string]string, error) {
	rec, err := m.store.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	// Non-nil even when the result is empty, since that is what tells the store to clear the column
	// rather than leave it alone.
	next := map[string]string{}
	if !replace {
		for k, v := range rec.Tags {
			next[k] = v
		}
	}
	for k, v := range set {
		next[k] = v
	}
	// After set, so removing and setting the same key in one call removes it. Either order is
	// defensible; this one is chosen because remove is the more specific instruction.
	for _, k := range remove {
		delete(next, k)
	}

	if err := m.store.Apply(ctx, name, store.Update{Tags: next}); err != nil {
		return nil, err
	}
	m.log.Info("tagged session", "session", name, "tags", tags.Format(next))

	if len(next) == 0 {
		// Reported the way the store reads it back, so a caller sees the same thing whether it
		// checks the response or lists afterwards.
		return nil, nil
	}
	return next, nil
}

// Clients reports how many clients are attached to a session.
func (m *Manager) Clients(name string) int64 {
	m.mu.Lock()
	sess, ok := m.sessions[name]
	m.mu.Unlock()
	if !ok {
		return 0
	}
	return sess.Clients()
}

// Kill terminates a session and forgets it.
//
// Without force, an unreachable shim is left recorded rather than forgotten: it may be
// busy rather than dead, and forgetting it would orphan a live shell. force exists for the
// case where the user knows better.
func (m *Manager) Kill(
	ctx context.Context, name string, force bool, sig int32,
) (surviving []int32, err error) {
	m.mu.Lock()
	sess, live := m.sessions[name]
	m.mu.Unlock()

	// Logged because this function used to log nothing at all, and it deletes the session record.
	//
	// That combination made a killed session indistinguishable from one killed from outside cm. All that
	// was left afterwards was the shim's "shim exiting" with exit_code=-1, which names neither the signal
	// nor who sent it, so after losing real work there was nothing to read. The shim logs its side too;
	// this is the side that knows a *request* arrived and what asked for it.
	m.log.Info("killing session", "session", name, "force", force, "signal", sig, "live", live)

	if live {
		// A session whose shell has already exited is not a failure to stop, whatever the RPC says.
		//
		// This window is narrow and real: the shell exits, the shim starts shutting down and closes its
		// connection, and the session is still in the registry. Shutdown then fails with a transport error,
		// and reporting that would be wrong twice over. The caller asked for the session to be gone and it
		// is, and returning early left the record in the database forever, since the delete below never ran.
		//
		// Found as a flaky `cm kill --all` reporting "stopping d5: ttrpc: closed" under -race, which widens
		// the window enough to hit. Checked after the call rather than before, because before is its own
		// race: the shell can exit between the check and the RPC.
		left, err := sess.Shutdown(ctx, force, sig)
		if err != nil && !force {
			// Two ways to recognize a session that has already gone, because they can arrive in either
			// order and only one of them is a state the server has caught up with.
			//
			// Ended() alone is not enough: the server learns of an exit by watching the output stream, so a
			// shim whose connection has already closed can report a transport error while this session
			// still looks alive. That surfaced as a flaky `cm kill --all` reporting "stopping s14: ttrpc:
			// closed" from a test that creates twenty rapidly-exiting sessions, which hits the window often
			// enough to fail a run in three. The resize path above already checks both for the same reason.
			ended, _ := sess.Ended()
			if !ended && !isSessionOver(err) && !isTransportClosed(err) {
				return nil, fmt.Errorf("stopping %s: %w", name, err)
			}
		}
		surviving = left
		// Give the shim a moment to exit so its socket is gone before returning, which
		// lets the name be reused immediately.
		select {
		case <-sess.Done():
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		return surviving, m.store.Delete(ctx, name)
	}

	rec, err := m.store.Get(ctx, name)
	if err != nil {
		return nil, err
	}

	// Not in the registry: try the shim directly in case another server started it.
	if alive, _ := probeShim(ctx, rec.ShimSocket); alive {
		conn, shim, dialErr := dialShim(rec.ShimSocket)
		if dialErr == nil {
			defer conn.Close()
			resp, err := shim.Shutdown(ctx, &shimv1.ShutdownRequest{
				Force:  force,
				Signal: sig,
			})
			// A shim that answered the probe and then vanished is the outcome the caller wanted, not a
			// failure to stop it. This path has no Session to ask about liveness, so the error is the only
			// evidence available -- which is why the transport check matters more here than above.
			//
			// This is where the flaky `cm kill --all` came from. A test creating twenty rapidly-exiting
			// sessions reaches Kill after the session left the registry but while its shim is still
			// shutting down, so the probe succeeds and the request loses the race, and cleanup reported
			// "stopping d14: ttrpc: closed" about one run in three.
			if err != nil && !force && !isSessionOver(err) && !isTransportClosed(err) {
				return nil, fmt.Errorf("stopping %s: %w", name, err)
			}
			if resp != nil {
				surviving = resp.SurvivingPids
			}
			return surviving, m.store.Delete(ctx, name)
		}
		if !force {
			return nil, fmt.Errorf("cannot reach shim for %s: %w", name, dialErr)
		}
	}

	if rec.State == store.StateRunning && !force {
		return nil, fmt.Errorf("shim for %s is unreachable; use --force to forget it", name)
	}
	return nil, m.store.Delete(ctx, name)
}

// ExpireDeadSessions removes dead sessions older than the configured age, along with their logs.
//
// Necessary rather than tidy: without it both the session list and the disk grow forever across
// reboots, and a machine that opens a session per terminal window accumulates them quickly.
//
// Only sessions that are not running are considered, and liveness is not re-probed here: a session
// recorded as dead has already been probed by Reconcile, and probing again would let a slow shim be
// deleted for being busy.
func (m *Manager) ExpireDeadSessions(ctx context.Context, now time.Time) (int, error) {
	if m.persist == nil || m.persist.ExpireAfter <= 0 {
		return 0, nil
	}

	records, err := m.store.List(ctx, "")
	if err != nil {
		return 0, fmt.Errorf("listing sessions to expire: %w", err)
	}

	cutoff := now.Add(-m.persist.ExpireAfter)
	// A session that saved no output is forgotten far sooner. See ForgetUnpersistedAfter.
	unpersistedCutoff := cutoff
	if m.persist.ForgetUnpersistedAfter > 0 {
		unpersistedCutoff = now.Add(-m.persist.ForgetUnpersistedAfter)
	}

	removed := 0
	for _, rec := range records {
		if rec.State == store.StateRunning {
			continue
		}
		// A live session in the registry is never expired, whatever the record says: the record can
		// lag, and deleting a session someone is attached to would be worse than keeping a stale
		// row.
		if _, live := m.Get(rec.Name); live {
			continue
		}
		// UpdatedAt rather than CreatedAt: what matters is how long ago the session stopped being
		// useful, not how long ago it started.
		// Keyed on whether persistence was *asked for*, not on whether a log exists. `cm run` writes
		// a log so its output survives the command, and treating that as a deliberately persisted
		// session would put every command a user runs in `cm list` for a week.
		limit := cutoff
		if !rec.PersistRequested {
			limit = unpersistedCutoff
		}
		if rec.UpdatedAt.After(limit) {
			continue
		}

		if rec.LogPath != "" {
			// A log that cannot be removed is not a reason to keep the record. The row is what makes
			// the session visible, and an orphaned file is a smaller problem than a session that
			// can never be cleaned up.
			if err := os.Remove(rec.LogPath); err != nil && !os.IsNotExist(err) {
				m.log.Warn("removing expired session log failed",
					"session", rec.Name, "path", rec.LogPath, "error", err)
			}
		}
		if err := m.store.Delete(ctx, rec.Name); err != nil {
			return removed, fmt.Errorf("expiring session %s: %w", rec.Name, err)
		}
		removed++
		m.log.Info("expired dead session", "session", rec.Name, "age", now.Sub(rec.UpdatedAt))
	}
	return removed, nil
}

// HistoryFromDisk renders a finished session's output by replaying its persisted log.
//
// Necessary because a session that ends leaves the registry, taking its terminal model with it, so
// there would otherwise be no way to read what a command printed once it exited. That is the common
// case for `cm run`, where the output is the whole point.
func (m *Manager) HistoryFromDisk(
	ctx context.Context, name string, format serverv1.HistoryFormat,
) ([]byte, error) {
	return m.replayFromDisk(ctx, name, func(term Terminal) ([]byte, error) {
		switch format {
		case serverv1.HistoryFormat_HISTORY_FORMAT_VT:
			return term.VT()
		case serverv1.HistoryFormat_HISTORY_FORMAT_HTML:
			return term.HTML()
		default:
			return term.Plain()
		}
	})
}

// ReadFromDisk renders the tail of a finished session's persisted output.
//
// The same replay as HistoryFromDisk with a different render, which is the point of splitting them: a
// finished command's output is the common case for `cm read`, since `cm run` waits for the command and
// the session is already gone by the time anything reads it.
func (m *Manager) ReadFromDisk(
	ctx context.Context, name string, lines int, unwrap, raw bool,
) ([]byte, error) {
	return m.replayFromDisk(ctx, name, func(term Terminal) ([]byte, error) {
		if raw {
			// unwrap is not passed on: it describes a rendering, and raw output does not go through one.
			return term.TailVT(lines)
		}
		return term.Tail(lines, unwrap)
	})
}

// RenderSnapshot turns a slice of a live session's output into text.
//
// Replayed into a throwaway terminal rather than rendered from the session's own model, and that is the
// point rather than an implementation detail: the session's model holds the *current* screen, which is
// what attached clients are looking at, so writing historical bytes into it would corrupt their view.
// A fresh model also means the result describes only the bytes asked for, instead of whatever the
// screen happened to contain.
//
// raw returns the bytes as the program emitted them, skipping the model entirely. Nothing needs
// rendering in that case, and building a terminal to hand back its input would only risk changing it.
func (m *Manager) RenderSnapshot(
	data []byte, rows, cols uint16, unwrap, raw bool,
) ([]byte, error) {
	if raw {
		return data, nil
	}
	if len(data) == 0 {
		return nil, nil
	}
	if m.newTerminal == nil {
		// Consistent with the other read paths: without an emulator the bytes cannot be rendered, and
		// handing back raw output where plain text was asked for would be wrong rather than degraded.
		return nil, errors.New("terminal rendering is unavailable, so output cannot be rendered")
	}
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}

	term, err := m.newTerminal(rows, cols)
	if err != nil {
		return nil, err
	}
	defer term.Close()

	if err := term.Write(data); err != nil {
		return nil, fmt.Errorf("replaying output: %w", err)
	}
	// Discard anything the emulator generated in response: those answer queries from a program that is
	// not listening to this model.
	term.TakePending()

	// Everything the slice produced, with no line bound. The slice is already bounded by the command
	// boundary it started at, so a line count would be a second, unrelated limit -- which is why the
	// CLI refuses to combine them.
	return term.Tail(0, unwrap)
}

// replayFromDisk rebuilds a finished session's screen from its saved log and hands it to render.
//
// Necessary because a session that ends leaves the registry, taking its terminal model with it, so there
// would otherwise be no way to read what a command printed once it exited.
func (m *Manager) replayFromDisk(
	ctx context.Context, name string, render func(Terminal) ([]byte, error),
) ([]byte, error) {
	rec, err := m.store.Get(ctx, name)
	if err != nil {
		return nil, err
	}
	if rec.LogPath == "" {
		return nil, fmt.Errorf(
			"session %s has ended and was not persisting, so its output is gone", name)
	}

	limits := seqlog.FileLimits{}
	if m.persist != nil {
		limits = m.persist.Limits
	}

	f, err := seqlog.OpenFile(rec.LogPath, limits)
	if err != nil {
		return nil, fmt.Errorf("opening persisted log for %s: %w", name, err)
	}
	defer f.Close()

	oldest, end := f.Bounds()
	if end == oldest {
		return nil, nil
	}
	data, _, err := f.ReadFrom(oldest)
	if err != nil {
		return nil, fmt.Errorf("reading persisted log for %s: %w", name, err)
	}

	if m.newTerminal == nil {
		// Without an emulator the bytes cannot be rendered. Returning them raw would be wrong for
		// plain text, which is what a caller piping this expects.
		return nil, fmt.Errorf("session %s has ended and terminal rendering is unavailable", name)
	}

	rows, cols := uint16(rec.Rows), uint16(rec.Cols)
	if rows == 0 || cols == 0 {
		rows, cols = 24, 80
	}
	term, err := m.newTerminal(rows, cols)
	if err != nil {
		return nil, err
	}
	defer term.Close()

	if err := term.Write(data); err != nil {
		return nil, fmt.Errorf("replaying output for %s: %w", name, err)
	}
	// Discard anything the emulator generated: those answer queries from a program that is gone.
	term.TakePending()

	return render(term)
}

// Close stops tracking sessions without terminating them.
//
// Shims are left running on purpose: that is what makes a server restart invisible to a
// running shell. The next server adopts them via Reconcile.
func (m *Manager) Close() error {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.sessions = make(map[string]*Session)
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for _, sess := range sessions {
		// Persist both resume points so the next server picks up exactly here. The client position is
		// stored alongside the shim one because they count the same output differently and the adopting
		// server needs both: one to resubscribe with, one to number its client log from.
		seq, clientSeq := sess.resumePoints()
		if err := m.store.Apply(ctx, sess.name,
			store.Update{LastSeq: &seq, ClientSeq: &clientSeq}); err != nil {
			// Losing this means the next server resubscribes from an older position and replays
			// output the client already saw, so it is worth knowing about.
			m.log.Error("recording resume point failed",
				"session", sess.name, "seq", seq, "error", err)
		}
		sess.Close()
	}
	return nil
}
