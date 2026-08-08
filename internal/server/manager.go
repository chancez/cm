package server

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// shimReadyTimeout bounds how long to wait for a freshly spawned shim to bind its socket.
const shimReadyTimeout = 10 * time.Second

// implicitPrefix names sessions the server allocates, as opposed to those a user names.
// Keeping them in one namespace makes them easy to list and reap.
const implicitPrefix = "s"

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

	mu       sync.Mutex
	sessions map[string]*Session
}

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
	}, nil
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
			state := store.StateDead
			if applyErr := m.store.Apply(ctx, rec.Name, store.Update{State: &state}); applyErr != nil {
				return fmt.Errorf("marking %s dead: %w", rec.Name, applyErr)
			}
			continue
		}

		// Resume from where the previous server stopped consuming.
		sess, err := m.adopt(ctx, rec, rec.LastSeq)
		if err != nil {
			// The shim answered a moment ago, so this is worth reporting but not fatal:
			// the remaining sessions should still come back.
			continue
		}
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
	conn, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		// ENOENT or ECONNREFUSED mean nothing is there. A timeout means unknown, so
		// report it as possibly alive to avoid discarding a busy shim.
		if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
			return true, err
		}
		return false, err
	}
	conn.Close()
	return true, nil
}

// adopt connects to an existing shim and starts consuming from fromSeq.
func (m *Manager) adopt(ctx context.Context, rec store.Session, fromSeq uint64) (*Session, error) {
	term, err := m.buildTerminal(uint16(rec.Rows), uint16(rec.Cols))
	if err != nil {
		return nil, err
	}
	sess, err := newSession(rec, term, fromSeq)
	if err != nil {
		if term != nil {
			term.Close()
		}
		return nil, err
	}
	go m.watch(sess)
	return sess, nil
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

// watch records a session's outcome once it ends and drops it from the registry.
func (m *Manager) watch(sess *Session) {
	<-sess.Done()

	ended, code := sess.Ended()
	_ = ended

	state := store.StateExited
	if code < 0 {
		// The shim vanished rather than the shell exiting, so the outcome is unknown.
		state = store.StateDead
		code = 0
	}
	seq := sess.LastSeq()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = m.store.Apply(ctx, sess.name, store.Update{
		State:    &state,
		ExitCode: &code,
		LastSeq:  &seq,
	})

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
	// Command overrides the user's shell for a new session.
	Command []string
	// Dir is the working directory for a new session.
	Dir string
	// Env holds extra KEY=VALUE entries for a new session.
	Env []string
	// Owned records that the attaching client claims the session, so dropping its
	// connection without detaching should end it.
	Owned bool
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

	// A record can exist without a live session: the shell exited, or a previous server
	// marked it dead. Reattaching to those is not possible, so the record is replaced.
	if rec, err := m.store.Get(ctx, opts.Name); err == nil {
		if rec.State == store.StateRunning {
			// Recorded as running but not in our registry, which happens if Reconcile
			// could not adopt it. Try once more before giving up.
			if alive, _ := probeShim(ctx, rec.ShimSocket); alive {
				sess, err := m.adopt(ctx, rec, rec.LastSeq)
				if err == nil {
					m.mu.Lock()
					m.sessions[opts.Name] = sess
					m.mu.Unlock()
					return sess, false, nil
				}
			}
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

// create spawns a shim and records the session.
func (m *Manager) create(ctx context.Context, opts OpenOptions) (*Session, error) {
	if opts.Rows == 0 || opts.Cols == 0 {
		opts.Rows, opts.Cols = 24, 80
	}

	socket := m.dirs.ShimSocket(opts.Name)
	if err := paths.CheckSocketPath(socket); err != nil {
		return nil, err
	}

	rec := store.Session{
		Name:       opts.Name,
		ShimSocket: socket,
		LogPath:    m.dirs.SessionLog(opts.Name),
		State:      store.StateRunning,
		Command:    strings.Join(opts.Command, " "),
		Cwd:        opts.Dir,
		Rows:       int(opts.Rows),
		Cols:       int(opts.Cols),
		Owned:      opts.Owned,
	}
	// Record before spawning so a shim can never exist without a row describing it. A row
	// with no shim is recoverable; a shim with no row is invisible.
	if err := m.store.Create(ctx, rec); err != nil {
		return nil, err
	}

	pid, err := m.spawnShim(ctx, opts, socket)
	if err != nil {
		_ = m.store.Delete(ctx, opts.Name)
		return nil, err
	}
	rec.ShimPID = pid
	if err := m.store.Apply(ctx, opts.Name, store.Update{ShimPID: &pid}); err != nil {
		return nil, err
	}

	sess, err := m.adopt(ctx, rec, 0)
	if err != nil {
		_ = m.store.Delete(ctx, opts.Name)
		return nil, err
	}

	// Record the shell's pid for reporting, best effort: the session works without it.
	if st, err := sess.State(ctx); err == nil {
		shellPID := int(st.ShellPid)
		_ = m.store.Apply(ctx, opts.Name, store.Update{ShellPID: &shellPID})
	}

	m.mu.Lock()
	m.sessions[opts.Name] = sess
	m.mu.Unlock()
	return sess, nil
}

// spawnShim re-execs this binary as a shim and waits for its socket.
//
// Go cannot fork, so re-exec replaces the double-fork a C implementation would use. The
// child is deliberately not waited on: it must outlive this server, so it is reparented to
// init by letting this process release it. Setsid detaches it from the server's session so
// a signal sent to the server's process group cannot reach it.
func (m *Manager) spawnShim(ctx context.Context, opts OpenOptions, socket string) (int, error) {
	args := []string{
		"--runtime-dir", m.dirs.Runtime,
		"--state-dir", m.dirs.State,
		"shim",
		"--session", opts.Name,
		"--rows", strconv.Itoa(int(opts.Rows)),
		"--cols", strconv.Itoa(int(opts.Cols)),
	}
	if opts.Dir != "" {
		args = append(args, "--dir", opts.Dir)
	}
	if len(opts.Command) > 0 {
		args = append(args, "--")
		args = append(args, opts.Command...)
	}

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
			records[i].LastSeq = sess.LastSeq()
		}
	}
	return records, nil
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
func (m *Manager) Kill(ctx context.Context, name string, force bool) error {
	m.mu.Lock()
	sess, live := m.sessions[name]
	m.mu.Unlock()

	if live {
		if err := sess.Shutdown(ctx, force); err != nil && !force {
			return fmt.Errorf("stopping %s: %w", name, err)
		}
		// Give the shim a moment to exit so its socket is gone before returning, which
		// lets the name be reused immediately.
		select {
		case <-sess.Done():
		case <-time.After(2 * time.Second):
		case <-ctx.Done():
		}
		return m.store.Delete(ctx, name)
	}

	rec, err := m.store.Get(ctx, name)
	if err != nil {
		return err
	}

	// Not in the registry: try the shim directly in case another server started it.
	if alive, _ := probeShim(ctx, rec.ShimSocket); alive {
		conn, shim, dialErr := dialShim(rec.ShimSocket)
		if dialErr == nil {
			defer conn.Close()
			if _, err := shim.Shutdown(ctx, &shimv1.ShutdownRequest{Force: force}); err != nil && !force {
				return fmt.Errorf("stopping %s: %w", name, err)
			}
			return m.store.Delete(ctx, name)
		}
		if !force {
			return fmt.Errorf("cannot reach shim for %s: %w", name, dialErr)
		}
	}

	if rec.State == store.StateRunning && !force {
		return fmt.Errorf("shim for %s is unreachable; use --force to forget it", name)
	}
	return m.store.Delete(ctx, name)
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
		// Persist the resume point so the next server picks up exactly here.
		seq := sess.LastSeq()
		_ = m.store.Apply(ctx, sess.name, store.Update{LastSeq: &seq})
		sess.Close()
	}
	return nil
}
