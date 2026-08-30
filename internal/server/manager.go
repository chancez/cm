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
	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/seq"
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
	records, err := m.store.List(ctx)
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
				"session", rec.ID, "socket", rec.ShimSocket, "error", err)
			state := store.StateDead
			if applyErr := m.store.Apply(ctx, rec.ID, store.Update{State: &state}); applyErr != nil {
				return fmt.Errorf("marking %s dead: %w", rec.ID, applyErr)
			}
			continue
		}

		// Resume from where the previous server stopped consuming.
		sess, err := m.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq, "")
		if err != nil {
			// The shim answered a moment ago, so this is worth reporting but not fatal:
			// the remaining sessions should still come back.
			m.log.Warn("adopting session failed",
				"session", rec.ID, "from_seq", rec.LastSeq, "error", err)
			continue
		}
		m.log.Info("adopted session", "session", rec.ID, "from_seq", rec.LastSeq)
		m.mu.Lock()
		m.sessions[rec.ID] = sess
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
// restoreFrom is a persisted log to seed the model from, for a session being revived after a reboot, and
// empty for an ordinary adoption. The two are alternatives rather than additions: an adopted session's
// history lives in a shim that is still running, a revived one's is on disk because its shim died with the
// machine. Both are seeded here, before newSession starts the pump, because content written to the model
// after that lands after whatever the new shell has already printed.
func (m *Manager) adopt(
	ctx context.Context, rec store.Session, fromSeq seq.Shim, clientSeq seq.Log, restoreFrom string,
) (*Session, error) {
	term, err := m.buildTerminal(uint16(rec.Rows), uint16(rec.Cols))
	if err != nil {
		return nil, err
	}
	// Filled by the replay below and handed to the session afterwards.
	//
	// The graphics store is only useful with a model to go with it, so it is built only when there is
	// one: an adopted session has to know the images its screen refers to, and newSession builds an
	// empty store, which is right for a session being created and wrong for one being adopted.
	//
	// The OSC trackers are recovered either way, which is why the replay is no longer conditional on a
	// terminal. They read the same bytes but need nothing from the emulator, and what they carry is what
	// `cm list` prints: the command the shell reported running, its last exit status, and any report a
	// shell integration wrote into the stream. All of it went blank on every restart while the evidence
	// sat in the shim's log unread.
	rp := replayed{}
	if term != nil {
		rp.gfx = graphics.NewStore(0)
		if restoreFrom != "" {
			limits := seqlog.FileLimits{}
			if m.persist != nil {
				limits = m.persist.Limits
			}
			// Not fatal, but the user asked for their content back and did not get it, so this is exactly
			// the kind of silent degradation the log exists for. ErrNothingToRestore is the ordinary case
			// for a session that never persisted and is not worth a line.
			if err := seedFromPersistedLog(restoreFrom, term, rp.gfx, limits); err != nil {
				if !errors.Is(err, ErrNothingToRestore) {
					m.log.Warn("replaying persisted session failed",
						"session", rec.ID, "path", restoreFrom, "error", err)
				}
			} else {
				m.log.Info("restored session from disk", "session", rec.ID, "path", restoreFrom)
			}
		}
	}
	// Outside the block above, and after the seed from disk rather than instead of it: the persisted log
	// rebuilds a screen from before this shim, and this replays what the shim itself still holds on top.
	if err := m.replayShimHistory(ctx, rec, fromSeq, term, &rp); err != nil {
		// A screen that could not be rebuilt is worth reporting, but the session works without
		// it: only restore and history are affected, which is where this started.
		m.log.Warn("rebuilding the screen for an adopted session failed",
			"session", rec.ID, "error", err)
	}
	// A record written before client_seq existed has zero there, which is indistinguishable from a
	// session that genuinely served nothing. Falling back to fromSeq is what an upgrade wants: it
	// reproduces the old behavior for that one adoption rather than starting the log at zero, which
	// would make every subscriber look like it had missed the entire session.
	if clientSeq == 0 {
		// The one deliberate crossing between the two spaces, and the conversion is what makes it say
		// so. Wrong by the rewrite drift for this single adoption, which self-corrects on the next
		// restart, and far better than starting the log at zero.
		clientSeq = seq.Log(fromSeq)
	}
	// Drained here rather than inside the option, so the report that survives is the one this function
	// decided on: see withRecoveredCommands on why leaving it in the tracker would republish it.
	fromLog, inLog := rp.reports.Take()
	reported := recoveredReport(rec, rp.commands, fromLog, inLog)
	sess, err := newSession(rec, term, fromSeq, clientSeq,
		withRecoveredCommands(rp.commands, rp.reports, reported))
	if err != nil {
		if term != nil {
			term.Close()
		}
		return nil, err
	}
	sess.log = m.log.With("session", rec.ID)
	// Labelled from the store, so a session adopted after a server restart is described by the name a
	// person knows it by rather than by its ID. Without this every message about an adopted session, and
	// every doctor finding, names something the user never typed.
	if names, err := m.Names(ctx, rec.ID); err == nil && len(names) > 0 {
		sess.setLabel(names[0])
	}
	sess.SetResizePolicy(m.resizePolicy)
	// The images the replay found, so a client attaching to this adopted session receives the transmissions
	// its screen's placements refer to. See recordGraphics.
	sess.setGraphicsStore(rp.gfx)

	// Persist what the shell reports about itself, so `list` and a terminal emulator opening a
	// new window see current values rather than whatever was true at creation.
	go m.persistMetadata(sess)

	go m.watch(sess)
	return sess, nil
}

// recoveredReport decides what a session being adopted last said about itself.
//
// Two sources, and they are not equivalent. A report in the retained output is cm's own OSC 25453,
// written by a shell integration; a stored one came from `cm report`, which is an RPC and leaves no trace
// in any stream. The log wins when it has one, because it is at least as new: the stored value was written
// while the previous server read that same stream, so anything still retained either produced it or came
// after it.
//
// The guard is the interesting part. A stored report is a claim nobody retracted, which is right for a
// program still sitting there blocked and wrong for one that finished during the restart. The replayed
// OSC 133 markers settle it: a shell back at a prompt with nothing running has no program in it to be
// blocked or busy, so a report about one is stale and is dropped rather than shown. Seen matters as much
// as the state, because "no markers at all" is not "at a prompt" -- it is a shell with no integration
// loaded, or a window whose markers scrolled out, and dropping a report on that evidence would forget
// every session that does not report OSC 133.
func recoveredReport(rec store.Session, cmds osc.CommandTracker, fromLog osc.Report, inLog bool) Reported {
	if inLog {
		r := Reported{State: fromLog.State, Detail: fromLog.Detail, Source: fromLog.Source}
		// The stored timestamp when it describes the same statement, since that is the accurate one. A
		// different statement is one nothing recorded the time of, so now is used: it is the moment cm
		// learned of it, and the only bound available for a sequence whose write time was never kept.
		if r.sameStatement(reportedFromStore(rec)) {
			r.At = rec.Reported.At
		} else {
			r.At = time.Now()
		}
		return r
	}

	stored := reportedFromStore(rec)
	if stored.State == "" {
		return Reported{}
	}
	if cmds.Seen() && !cmds.State().Running {
		return Reported{}
	}
	return stored
}

// reportedFromStore converts a stored report into the server's own shape.
func reportedFromStore(rec store.Session) Reported {
	return Reported{
		State:  rec.Reported.State,
		Detail: rec.Reported.Detail,
		Source: rec.Reported.Source,
		At:     rec.Reported.At,
	}
}

// replayed is what a replay of a shim's retained output recovered.
//
// A struct rather than more parameters because these are all answers to the same question -- what did the
// previous server know that this one has to rebuild -- and they are filled from a single pass over the
// bytes.
type replayed struct {
	// gfx holds the image transmissions the replay found, nil when there is no terminal model to go
	// with them.
	gfx *graphics.Store
	// commands and reports are the OSC trackers, left holding both the state the markers described and
	// any fragment of a sequence that was still arriving at the replay's end.
	commands osc.CommandTracker
	reports  osc.ReportTracker
}

// replayShimHistory feeds a shim's retained output into a terminal model and the OSC trackers, up to
// fromSeq.
//
// Stops at fromSeq because that is where the session's own pump takes over. Replaying past it would
// write the same bytes twice, and a terminal fed duplicate output shows duplicated lines.
//
// Writes only to the model, never to the session's client log: these bytes are history that clients
// either already saw or will receive as part of a restored screen. Appending them would replay old
// output to an attached client as though it were new.
//
// term may be nil, in which case only rp is filled. What the trackers derive needs no emulator, and a
// session's state column should not depend on whether this build has one.
func (m *Manager) replayShimHistory(
	ctx context.Context, rec store.Session, fromSeq seq.Shim, term Terminal, rp *replayed,
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
	if seq.Shim(st.OldestSeq) >= fromSeq {
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

	// One scanner across the whole replay, because a transmission's payload is chunked and it reassembles
	// across calls. A scanner per chunk would see fragments and record nothing.
	var replayScanner graphics.Scanner

	for {
		out, err := sub.Recv()
		if err != nil {
			// Reaching the end of what is retained is the normal exit, not a failure: whatever was
			// written before this point is what the screen is rebuilt from.
			break
		}
		data := out.Data
		// Trim the tail that the pump will deliver, so the boundary byte is written exactly once.
		chunkStart := seq.Shim(out.Seq)
		if end := chunkStart + seq.Shim(len(data)); end > fromSeq {
			if chunkStart >= fromSeq {
				break
			}
			data = data[:fromSeq-chunkStart]
		}
		// Recorded into cm's own image store as well as fed to the model, and the second half is not
		// optional. The model regains its images from this replay, but libghostty's formatter does not
		// re-emit them, so what a client actually receives on attach comes from this store. Without it a
		// client attaching after a restart got a screen of placements with nothing to resolve, and the image
		// was blank.
		//
		// Bookkeeping only: the segments are not re-emitted anywhere, so a transfer resolved here goes into
		// the store and nothing else. That is why this uses recordGraphics rather than handleGraphics, which
		// also builds the byte stream for clients.
		if rp.gfx != nil {
			for _, seg := range replayScanner.Scan(data) {
				if !seg.Graphics {
					continue
				}
				resolved, err := graphics.ReadTransfer(seg.Cmd)
				if err != nil {
					// A file-backed transfer whose file has gone since. Nothing to record, and nothing to
					// report either: the image was already on the model's screen or it was not.
					continue
				}
				recordGraphics(rp.gfx, resolved)
			}
		}
		// Fed the same bytes the pump would have, in the same order and before the graphics handling and
		// the prompt rewrite, so the markers are read exactly as the shell wrote them. See feedTerminal,
		// which does this for live output; this is the same two lines against history.
		rp.commands.Feed(data)
		rp.reports.Feed(data)
		if term != nil {
			if err := term.Write(data); err != nil {
				return fmt.Errorf("replaying output: %w", err)
			}
		}
		if chunkStart+seq.Shim(len(data)) >= fromSeq {
			break
		}
	}

	// Discard anything the emulator generated in response: those answer queries from a program that
	// asked before this server existed, and nothing is waiting for the replies.
	if term != nil {
		term.TakePending()
	}
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
			// Written on every publish rather than only when it changed, which costs nothing: the row is
			// already being updated, and the report carries its own timestamp, so re-writing the same
			// values cannot make it look fresher than it is.
			//
			// This is the only copy. A report from `cm report` arrives as an RPC and never touches the
			// output stream, so unlike everything else in this struct it cannot be replayed from the
			// shim's log. See store.Session.Reported.
			upd.Reported = &store.ReportedState{
				State:  meta.Reported.State,
				Detail: meta.Reported.Detail,
				Source: meta.Reported.Source,
				At:     meta.Reported.At,
			}
			if err := m.store.Apply(ctx, sess.id, upd); err != nil {
				// Advisory, so the session continues. Logged because a `list` showing a stale
				// directory is otherwise unexplainable.
				m.log.Warn("recording session metadata failed",
					"session", sess.id, "error", err)
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
		if m.sessions[sess.id] == sess {
			delete(m.sessions, sess.id)
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
		// Genuinely unknown: the shim could not obtain a status at all, so there is nothing to report.
		//
		// Only that. A shell killed by a signal used to arrive here too, because Go's ExitCode returns -1
		// for "terminated by a signal" as well as for "has not exited", so a tracked session killed by
		// SIGTERM was recorded as a lost one with exit code 0 and `cm ls` reported success for it. The shim
		// now reports 128+signal, the shell convention, so a negative code means only what this branch
		// says.
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
		if err := m.store.Delete(ctx, sess.id); err != nil {
			// Not fatal: the record simply expires on the usual schedule instead, which is the behavior
			// this replaces.
			m.log.Warn("forgetting an exited interactive session failed",
				"session", sess.id, "error", err)
		} else {
			m.releaseNames(ctx, sess.id)
			m.log.Info("forgot exited interactive session",
				"session", sess.id, "exit_code", code)
		}

		m.mu.Lock()
		if m.sessions[sess.id] == sess {
			delete(m.sessions, sess.id)
		}
		m.mu.Unlock()
		return
	}

	shimSeq, clientSeq := sess.resumePoints()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.store.Apply(ctx, sess.id, store.Update{
		State:     &state,
		ExitCode:  &code,
		LastSeq:   &shimSeq,
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
				"session", sess.id, "state", state)
		} else {
			m.log.Error("recording session outcome failed",
				"session", sess.id, "state", state, "error", err)
		}
	}
	m.log.Info("session ended", "session", sess.id, "state", state, "exit_code", code)

	m.mu.Lock()
	if m.sessions[sess.id] == sess {
		delete(m.sessions, sess.id)
	}
	m.mu.Unlock()
}

// OpenOptions describes an attach request that may need to create the session.
type OpenOptions struct {
	// Ref is what the caller asked for: a name, an "@id" reference, or empty for a session with no
	// name at all. Resolving it is Open's first job.
	//
	// Empty used to mean "allocate a name like s17". It now means what it says: a session identified
	// only by its ID, since an implicit name was never anything but a stand-in for an identity.
	Ref string
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
	// id is the identity allocated for a session being created. Set by Open, never by a caller.
	id string
	// name is Ref once it is known to be a name rather than an ID reference, so it can be bound to the
	// new session and matched against the persistence patterns. Empty for a session with no name.
	name string
	// ClientEnv holds terminal-related variables from the attaching client, recorded so a shell
	// in the session can refresh them later.
	ClientEnv map[string]string
	// ReadOnly marks a caller that only observes: `cm read --follow`, `cm attach --read-only`, and the
	// follower half of `cm send --follow`.
	//
	// It exists here for one decision, that such a caller must not revive a dead session. See
	// openExisting. Everything else read-only implies is a property of the attachment rather than of
	// opening the session, and lives on the token instead.
	ReadOnly bool
}

// EndedSessionError reports that a read-only caller asked for a session whose shell is gone.
//
// Carries what the reply needs rather than only saying so, because the caller is Attach and the answer it
// has to send is an Opened followed by an Exited: a follower is told the same thing whether the shell
// exited while it was watching or before it arrived. Without the fields it would have to re-read the
// record it was just refused on.
//
// Reports as ErrSessionGone so existing checks keep working.
type EndedSessionError struct {
	ID       string
	Label    string
	ExitCode int
	LastSeq  seq.Shim
}

func (e *EndedSessionError) Error() string {
	return fmt.Sprintf("%s: %s", e.ID, ErrSessionGone)
}

func (e *EndedSessionError) Is(target error) bool { return target == ErrSessionGone }

// Open returns the session a reference names, creating one when a name holds nothing yet.
//
// This is where a name being a binding rather than an identity pays for itself. Attaching, creating, and
// reviving are all the same operation now: resolve the reference, and if it resolves to nothing at all,
// allocate an identity and point the name at it. What used to be three code paths keyed on whether a
// record existed and what state it was in is one path with one decision in it.
func (m *Manager) Open(ctx context.Context, opts OpenOptions) (*Session, bool, error) {
	if opts.Ref == "" {
		// No name asked for, so none is bound. The session is reachable by ID, which is what makes
		// this safe: a session with no names is still addressable rather than stranded.
		return m.createWithID(ctx, opts)
	}

	value, isID := paths.SessionRef(opts.Ref)
	if isID {
		if err := paths.ValidateSessionID(value); err != nil {
			return nil, false, err
		}
		// An ID that names no record at all is a stale reference, and openExisting reports it as such.
		// Deliberately not created: an ID is allocated by cm rather than chosen, so a caller holding one
		// cm has never issued is holding something from a database that is gone, and inventing a session
		// for it would put them somewhere they did not ask to be.
		return m.openExisting(ctx, opts, value)
	}

	if err := paths.ValidateSessionName(value); err != nil {
		return nil, false, err
	}
	opts.name = value

	binding, err := m.store.Binding(ctx, value)
	switch {
	case errors.Is(err, store.ErrNotFound):
		// A name nothing holds: allocate an identity and point the name at it. The name owns the
		// session it was created with, so killing by that name kills the session, which is what
		// `cm kill work` has always meant.
		return m.createAndBind(ctx, opts, store.KillTarget)
	case err != nil:
		return nil, false, err
	}

	sess, created, err := m.openExisting(ctx, opts, binding.SessionID)
	if errors.Is(err, store.ErrNotFound) {
		// A name pointing at a record that is not there at all. Nothing removes a record without its
		// names, so this should not happen, and refusing would leave the name permanently unusable while
		// looking like the session it named is merely missing. Healed by creating one and moving the name,
		// and logged rather than done quietly, since the state itself is a bug somewhere.
		m.log.Warn("name pointed at a session with no record; creating a new one",
			"name", value, "missing_session", binding.SessionID)
		return m.createAndBind(ctx, opts, binding.OnKill)
	}
	return sess, created, err
}

// openExisting returns the session with this ID, reviving it when its shell is gone.
//
// Reviving keeps the ID rather than allocating a new one, which is the point: an ID is the handle cm
// hands out and it has to stay usable, so `cm attach @a7k2m9x4` works on a session whose shell exited for
// the same reason attaching by name does. The alternative was tried first and it forced attach-by-ID to
// refuse outright, since it would have had to return a session whose ID was not the one asked for.
//
// What an ID promises is that it is never handed to a session that is not the continuation of the one it
// named. A revive satisfies that: same record, same content replayed from the same log, a new shell. The
// stronger reading -- that an ID names one shell and never a second -- bought nothing and cost the ability
// to attach by ID at all. It also leaked: a new ID meant a new log path, so the old log was orphaned with
// its record deleted, and expiry removes a log through the record that names it.
//
// Returns ErrNotFound when no record exists, and does not create. The caller decides what that means,
// because the two references differ: a name that resolves to nothing is healed by creating a session,
// while an ID that resolves to nothing is stale.
func (m *Manager) openExisting(
	ctx context.Context, opts OpenOptions, id string,
) (*Session, bool, error) {
	// Live in this server's registry is the common case, and by far the cheapest.
	m.mu.Lock()
	sess, ok := m.sessions[id]
	m.mu.Unlock()
	if ok {
		sess.setLabel(opts.name)
		return sess, false, nil
	}

	rec, err := m.store.Get(ctx, id)
	if err != nil {
		return nil, false, err
	}

	if rec.State == store.StateRunning {
		// Recorded as running but not in our registry, which happens if Reconcile could not adopt it.
		// Try once more before giving up: only ENOENT is conclusive, and a shim that was merely busy is
		// still holding a live shell.
		if alive, _ := probeShim(ctx, rec.ShimSocket); alive {
			sess, err := m.adopt(ctx, rec, rec.LastSeq, rec.ClientSeq, "")
			if err == nil {
				sess.setLabel(opts.name)
				m.mu.Lock()
				m.sessions[rec.ID] = sess
				m.mu.Unlock()
				return sess, false, nil
			}
		}
	}

	// An observer is told the session ended rather than being given a new shell.
	//
	// The revive below is deliberate for an interactive attach, and wrong for a reader. `cm read --follow`
	// on a session whose shell had exited started a fresh shell under it and then streamed that, so a read
	// command silently resurrected the thing it was asked to report on, and never returned: the session it
	// was following was alive again by its own doing, so no exit was ever coming. It hung until --timeout,
	// and without one it hung forever.
	//
	// Reported as ErrSessionGone, which the Attach handler already turns into Opened followed by Exited,
	// because that is the same answer a follower gets when the shell exits while it is watching. One
	// answer for one situation, whichever side of the attach the exit fell on.
	if opts.ReadOnly {
		return nil, false, &EndedSessionError{
			ID:       rec.ID,
			Label:    opts.name,
			ExitCode: rec.ExitCode,
			LastSeq:  seq.Shim(rec.LastSeq),
		}
	}

	// Nothing to attach to, so a new shell starts under this ID with the old one's content replayed.
	//
	// Destructive and once silent: attaching to a session whose shell had exited started a fresh shell
	// under it, and the exit status and pid that were the only evidence the previous run happened went
	// with the deleted row. Logged rather than refused, because refusing would break the case this path
	// exists for: a terminal emulator restoring a saved window attaches by name and must get a working
	// session whether or not the previous one is still alive. What it must not do is pretend nothing was
	// there.
	opts = m.inheritForRestore(opts, rec)
	m.log.Info("replacing an ended session with a new shell",
		"session", rec.ID, "name", opts.name, "previous_state", rec.State,
		"previous_exit_code", rec.ExitCode, "previous_shell_pid", rec.ShellPID,
		"content_restored", opts.restoreFrom != "")
	if err := m.store.Delete(ctx, rec.ID); err != nil {
		return nil, false, fmt.Errorf("replacing stale record for %s: %w", rec.ID, err)
	}

	// The same ID, so the socket and the log keep their paths. The wait for the previous shim's socket
	// in create is what makes that safe, and this is the case it was written for.
	opts.id = rec.ID
	sess, err = m.create(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	sess.setLabel(opts.name)
	return sess, true, nil
}

// createAndBind allocates an identity, creates the session, and points a name at it.
func (m *Manager) createAndBind(
	ctx context.Context, opts OpenOptions, onKill store.KillAction,
) (*Session, bool, error) {
	sess, created, err := m.createWithID(ctx, opts)
	if err != nil {
		return nil, false, err
	}
	sess.setLabel(opts.name)
	if err := m.store.Bind(ctx, store.Binding{
		Name:      opts.name,
		SessionID: sess.id,
		OnKill:    onKill,
	}); err != nil {
		return nil, false, fmt.Errorf("binding %s to %s: %w", opts.name, sess.id, err)
	}
	m.log.Info("bound a new name", "name", opts.name, "session", sess.id, "on_kill", onKill)
	return sess, created, nil
}

// createWithID allocates an identity and creates the session under it.
func (m *Manager) createWithID(ctx context.Context, opts OpenOptions) (*Session, bool, error) {
	id, err := m.store.NewID(ctx)
	if err != nil {
		return nil, false, err
	}
	opts.id = id
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

	socket := m.dirs.ShimSocket(opts.id)
	if err := paths.CheckSocketPath(socket); err != nil {
		return nil, err
	}

	// Wait for a previous shim on this socket to go before spawning one that has to bind it.
	//
	// A shim stays reachable briefly after its shell exits, deliberately: it is the only thing that knows
	// the exit status, so it lingers for exitGrace to be asked. Reviving a session under the same ID inside
	// that window meant the new shim found the socket taken and died with "already served by a live shim",
	// while waitForShim below dialed the *old* shim, got an answer, and reported the new one ready.
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
		return nil, fmt.Errorf("waiting for the previous shim for %s to exit: %w", opts.id, err)
	}

	// Either reason produces a log; only the first makes the session long-lived.
	// Matched against the name rather than the ID, since a pattern like "kitty.*" is about what a
	// session is called. A session with no name is only persisted when it is asked for explicitly,
	// which is the honest reading: there is nothing for a pattern to match.
	requested := m.persistsSession(opts.name, opts.Persist)
	persisting := requested || opts.CaptureOutput

	logPath := ""
	if persisting {
		logPath = m.dirs.SessionLog(opts.id)
	}

	rec := store.Session{
		ID:         opts.id,
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
		_ = m.store.Delete(ctx, opts.id)
		return nil, err
	}
	rec.ShimPID = pid
	if err := m.store.Apply(ctx, opts.id, store.Update{ShimPID: &pid}); err != nil {
		return nil, err
	}

	sess, err := m.adopt(ctx, rec, 0, 0, opts.restoreFrom)
	if err != nil {
		_ = m.store.Delete(ctx, opts.id)
		return nil, err
	}

	// Record the shell's pid for reporting. Inline first, so a listing run immediately after this shows
	// it, and retried in the background when that fails.
	//
	// Retried rather than left alone, which is what it did: this is one RPC to a shim that has just
	// started, nothing else ever writes the field, and `cm list` reads it straight from the record. So a
	// single transient failure meant a healthy session reported pid 0 for the rest of its life. It
	// surfaced as an e2e test waiting for a live pid and timing out with no other symptom, under two test
	// suites running at once, which is the sort of load that makes one RPC to a starting process fail.
	if !m.recordShellPID(ctx, sess) {
		go m.retryShellPID(sess)
	}

	// Logged with the name as well as the ID, and this is the line that ties the two together: every
	// other line about this session carries only the ID, which is stable, so a reader looking for
	// "what was work" needs one place that says.
	m.log.Info("created session",
		"session", opts.id, "name", opts.name, "shim_pid", pid, "persisting", persisting,
		"rows", opts.Rows, "cols", opts.Cols)

	m.mu.Lock()
	m.sessions[opts.id] = sess
	m.mu.Unlock()
	return sess, nil
}

// shellPIDAttempts and shellPIDRetryDelay bound the retry above.
//
// Bounded rather than indefinite: a shim that cannot answer after this is not going to, and a goroutine
// polling one forever would outlive every reason to care. Five seconds in total, which is generous against
// what it is waiting for, since the shim answered a readiness dial before this ran.
const (
	shellPIDAttempts   = 10
	shellPIDRetryDelay = 500 * time.Millisecond
)

// recordShellPID asks a session's shim for its shell's pid and stores it, reporting whether it landed.
func (m *Manager) recordShellPID(ctx context.Context, sess *Session) bool {
	st, err := sess.State(ctx)
	if err != nil {
		m.log.Warn("reading shim state failed", "session", sess.id, "error", err)
		return false
	}
	if st.ShellPid <= 0 {
		// A shim that reports no shell has nothing to record yet. Not an error: it answers before its
		// shell is necessarily up.
		return false
	}
	shellPID := int(st.ShellPid)
	if err := m.store.Apply(ctx, sess.id, store.Update{ShellPID: &shellPID}); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			// The record is gone, so the session has been removed and there is nothing left to describe.
			// Treated as done rather than retried, or the retry would run until it gave up on every
			// short-lived session.
			return true
		}
		m.log.Warn("recording shell pid failed", "session", sess.id, "error", err)
		return false
	}
	return true
}

// retryShellPID keeps trying to record the shell pid until it lands, the session ends, or it gives up.
func (m *Manager) retryShellPID(sess *Session) {
	for range shellPIDAttempts {
		select {
		case <-sess.Done():
			return
		case <-time.After(shellPIDRetryDelay):
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		ok := m.recordShellPID(ctx, sess)
		cancel()
		if ok {
			return
		}
	}
	// Said out loud, because the visible result is a session listed with pid 0 and nothing else to explain
	// it, which is the state this retry exists to avoid.
	m.log.Warn("gave up recording the shell pid; this session will report pid 0", "session", sess.id)
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
		"--session", opts.id,
		// Spelled out rather than left for the shim to derive, because only this side knows the value
		// above is an identity. A shim that added the sigil itself exported `@name` under an older server.
		"--session-ref", paths.FormatSessionID(opts.id),
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
		return 0, fmt.Errorf("spawning shim for %s: %w", opts.id, err)
	}
	pid := cmd.Process.Pid

	// Release the child rather than waiting: it outlives this server by design. Without
	// this the process would remain a zombie until the server exits.
	go func() { _ = cmd.Wait() }()

	if err := waitForShim(ctx, socket); err != nil {
		return pid, fmt.Errorf("shim for %s did not become ready: %w", opts.id, err)
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
func (m *Manager) Get(id string) (*Session, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	sess, ok := m.sessions[id]
	return sess, ok
}

// List returns session records, with live details filled in from the registry.
func (m *Manager) List(ctx context.Context) ([]store.Session, error) {
	records, err := m.store.List(ctx)
	if err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range records {
		if sess, ok := m.sessions[records[i].ID]; ok {
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
	ctx context.Context, id string, set map[string]string, remove []string, replace bool,
) (map[string]string, error) {
	rec, err := m.store.Get(ctx, id)
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

	if err := m.store.Apply(ctx, id, store.Update{Tags: next}); err != nil {
		return nil, err
	}
	m.log.Info("tagged session", "session", id, "tags", tags.Format(next))

	if len(next) == 0 {
		// Reported the way the store reads it back, so a caller sees the same thing whether it
		// checks the response or lists afterwards.
		return nil, nil
	}
	return next, nil
}

// Clients reports how many clients are attached to a session.
func (m *Manager) Clients(id string) int64 {
	m.mu.Lock()
	sess, ok := m.sessions[id]
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
	ctx context.Context, id string, force bool, sig int32,
) (surviving []int32, err error) {
	m.mu.Lock()
	sess, live := m.sessions[id]
	m.mu.Unlock()

	// Logged because this function used to log nothing at all, and it deletes the session record.
	//
	// That combination made a killed session indistinguishable from one killed from outside cm. All that
	// was left afterwards was the shim's "shim exiting" with exit_code=-1, which names neither the signal
	// nor who sent it, so after losing real work there was nothing to read. The shim logs its side too;
	// this is the side that knows a *request* arrived and what asked for it.
	m.log.Info("killing session", "session", id, "force", force, "signal", sig, "live", live)

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
			if !ended && !isSessionOver(err) && !isTransportClosed(err) && !isProcessGone(err) {
				return nil, fmt.Errorf("stopping %s: %w", id, err)
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
		m.releaseNames(ctx, id)
		return surviving, m.store.Delete(ctx, id)
	}

	rec, err := m.store.Get(ctx, id)
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
			if err != nil && !force && !isSessionOver(err) && !isTransportClosed(err) &&
				!isProcessGone(err) {
				return nil, fmt.Errorf("stopping %s: %w", id, err)
			}
			if resp != nil {
				surviving = resp.SurvivingPids
			}
			m.releaseNames(ctx, id)
			return surviving, m.store.Delete(ctx, id)
		}
		if !force {
			return nil, fmt.Errorf("cannot reach shim for %s: %w", id, dialErr)
		}
	}

	if rec.State == store.StateRunning && !force {
		return nil, fmt.Errorf("shim for %s is unreachable; use --force to forget it", id)
	}
	m.releaseNames(ctx, id)
	return nil, m.store.Delete(ctx, id)
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

	records, err := m.store.List(ctx)
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
		if _, live := m.Get(rec.ID); live {
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
					"session", rec.ID, "path", rec.LogPath, "error", err)
			}
		}
		m.releaseNames(ctx, rec.ID)
		if err := m.store.Delete(ctx, rec.ID); err != nil {
			return removed, fmt.Errorf("expiring session %s: %w", rec.ID, err)
		}
		removed++
		m.log.Info("expired dead session", "session", rec.ID, "age", now.Sub(rec.UpdatedAt))
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

	f, err := seqlog.OpenFile[seq.Shim](rec.LogPath, limits)
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
		shimSeq, clientSeq := sess.resumePoints()
		if err := m.store.Apply(ctx, sess.id,
			store.Update{LastSeq: &shimSeq, ClientSeq: &clientSeq}); err != nil {
			// Losing this means the next server resubscribes from an older position and replays
			// output the client already saw, so it is worth knowing about.
			m.log.Error("recording resume point failed",
				"session", sess.id, "seq", shimSeq, "error", err)
		}
		sess.Close()
	}
	return nil
}
