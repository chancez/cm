package server

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// Finding is one thing wrong with an installation.
type Finding struct {
	// Kind is what sort of problem this is, as a stable string a script can match on.
	Kind string
	// Session is the session it concerns, from the socket name or the shim itself.
	Session string
	// Socket is the path involved.
	Socket string
	// ShimPID and ShellPID are set when a shim answered and reported them.
	ShimPID  int
	ShellPID int
	// Detail explains the finding in a sentence.
	Detail string
	// Fixable reports whether Repair can act on it.
	Fixable bool
}

// Finding kinds. Stable strings rather than an enum, so `cm doctor --json` stays parseable as the set grows.
const (
	// FindingOrphanShim is a live shim with no session record: it holds a pty and a shell that nothing
	// knows about, and nothing will ever reattach to.
	FindingOrphanShim = "orphan-shim"
	// FindingStaleSocket is a socket file nothing answers on, left by a shim that died without unlinking.
	FindingStaleSocket = "stale-socket"
	// FindingMissingShim is a session recorded as running whose shim cannot be reached, so the record
	// promises something that is not there.
	FindingMissingShim = "missing-shim"
)

// Diagnose reports problems with an installation, without changing anything.
//
// Worth having as a command rather than only as internal bookkeeping, because the failure it looks for is
// silent and cumulative. A shim holds a pty for as long as it runs; macOS caps them at 511 system-wide. A
// leak of one per session is invisible until the limit is reached, at which point the symptom is an
// unrelated program failing to allocate a terminal. Nothing in cm's normal output would have shown 426 stray
// shims, which is how many had accumulated before this was written.
//
// Scoped to cm's own runtime directory and its own database. That is a deliberate limit: it cannot see a
// shim whose runtime directory has been deleted, because there is nothing left to enumerate. The
// alternative, scanning the process table for anything that looks like a shim, can be fooled and could kill
// something that is not cm's, which is a worse failure than missing an orphan.
//
// The client's capabilities are passed alongside its version because only the client knows either, and
// because a version difference on its own cannot say what breaks. See checkCapabilitySkew.
func (m *Manager) Diagnose(
	ctx context.Context, clientVersion string, clientCaps capability.Set,
) ([]Finding, error) {
	records, err := m.store.List(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing sessions: %w", err)
	}
	known := make(map[string]store.Session, len(records))
	for _, rec := range records {
		known[rec.ID] = rec
	}

	sockets, err := m.shimSockets()
	if err != nil {
		return nil, err
	}

	var findings []Finding
	seen := make(map[string]bool, len(sockets))
	// Collected from the probes below rather than by probing again, since each shim is already being
	// asked about itself here and a second round trip per session would double the cost of the command.
	shimVersions := make(map[string]string, len(sockets))
	shimCaps := make(map[string]capability.Set, len(sockets))

	for _, sock := range sockets {
		id := sessionIDFromSocket(sock)
		seen[id] = true

		st, alive := probeShimState(ctx, sock)
		if alive {
			// Recorded even when empty. A shim too old to report a version is itself skew of at least
			// that much, and the check below says so rather than treating it as agreement.
			shimVersions[id] = st.Version
			// Empty parses to the zero Set, which answers Unknown rather than Absent, so a shim predating
			// capability reporting is not mistaken for one that refused everything.
			shimCaps[id] = capability.Parse(st.GetCapabilities())
		}
		switch {
		case !alive:
			// Nothing answers. Either a shim died without unlinking, or one is mid-startup; the caller is
			// told which is likelier by whether a record exists.
			detail := "socket file with nothing listening, left by a shim that died"
			if _, ok := known[id]; ok {
				detail = "socket file with nothing listening, though a session record exists"
			}
			findings = append(findings, Finding{
				Kind: FindingStaleSocket, Session: id, Socket: sock,
				Detail: detail, Fixable: true,
			})

		case known[id].ID == "":
			// A shim is running and serving a session nothing knows about. This is the one that costs
			// resources: it holds a pty and a shell that no client can ever reach.
			f := Finding{
				Kind: FindingOrphanShim, Session: id, Socket: sock,
				ShimPID: int(st.ShimPid), ShellPID: int(st.ShellPid),
				Detail:  "shim is running with no session record, so nothing can reattach to it",
				Fixable: true,
			}
			if st.Exited {
				f.Detail = "shim is running with no session record, and its shell has already exited"
			}
			findings = append(findings, f)
		}
	}

	// The reverse: a record promising a shim that is not there. Not fixable here, since Reconcile already
	// marks these dead on startup and expiry removes them on its own schedule; reporting is enough.
	for _, rec := range records {
		if rec.State != store.StateRunning || seen[rec.ID] {
			continue
		}
		findings = append(findings, Finding{
			Kind: FindingMissingShim, Session: rec.ID, Socket: rec.ShimSocket,
			Detail: "recorded as running but has no socket, so its shim is gone",
		})
	}

	// The rest of the checks, each encoding a failure that has happened here. They are independent and
	// non-destructive, so they all run rather than stopping at the first thing found: a reader debugging a
	// problem wants the whole picture, not the first item alphabetically.
	findings = append(findings, m.checkVersionSkew(clientVersion)...)
	findings = append(findings, m.checkCapabilitySkew(clientCaps)...)
	findings = append(findings, m.checkShimVersionSkew(shimVersions, shimCaps)...)
	findings = append(findings, m.checkTerminal()...)
	findings = append(findings, m.checkEmulatorSpeed()...)
	findings = append(findings, m.checkDeniedModes()...)
	findings = append(findings, m.checkSocketPath()...)
	findings = append(findings, m.checkShellIntegration()...)
	findings = append(findings, m.checkSessionBacklog(ctx)...)
	findings = append(findings, m.checkPtyPressure()...)
	findings = append(findings, m.checkDirPerms()...)
	findings = append(findings, m.checkMissingLogs(ctx)...)
	findings = append(findings, m.checkTrackedShims(ctx)...)
	findings = append(findings, m.checkServerSocket()...)
	// Last, and with the clock passed in rather than read inside, so a test can place entries relative to a
	// fixed now instead of sleeping.
	findings = append(findings, m.checkLogs(time.Now())...)

	sort.Slice(findings, func(i, j int) bool {
		if findings[i].Kind != findings[j].Kind {
			return findings[i].Kind < findings[j].Kind
		}
		return findings[i].Session < findings[j].Session
	})
	return findings, nil
}

// Repair acts on the fixable findings and reports what it did.
//
// Shuts an orphan down through its own socket rather than signalling its process. A shim asked to shut down
// closes its pty and reaps its shell; killing the process leaves the shell to be reparented and keeps
// running. Doing it politely is also what makes this safe to run against a live installation: a shim that
// is not an orphan is never contacted.
func (m *Manager) Repair(ctx context.Context, findings []Finding) []string {
	var done []string
	for _, f := range findings {
		if !f.Fixable {
			continue
		}
		switch f.Kind {
		case FindingOrphanShim:
			// Logged before the attempt, since this force-kills a shell. An orphan is by definition
			// something no client is watching, but "orphan" is a judgement this code made, and if it was
			// wrong the shell it took down was someone's. Recording the decision separately from the
			// outcome is what makes that reviewable afterwards.
			m.log.Info("shutting down an orphaned shim",
				"session", f.Session, "socket", f.Socket, "shim_pid", f.ShimPID)
			// A shim that took the request and then dropped the connection did what was asked. The
			// transport carries the reply, not the shutdown: by the time one can be lost the shell has
			// already been signalled, so treating the error as failure reports the opposite of what
			// happened. `cm kill` reaches the same conclusion in two places for the same reason, via
			// isTransportClosed.
			//
			// This is the whole finding a reader acts on, which is what made it worth fixing here rather
			// than only in the shim. `cm doctor --repair` printing "0 things" after reaping an orphan tells
			// an operator a shell is still leaked, and the next move for a leaked pty is to go hunting for a
			// process that is already gone. Surfaced as TestRepairStopsOrphansAndSparesHealthySessions
			// failing about 1 run in 8 under parallel load with "Repair() did 0 things", where the discarded
			// warning said `ttrpc: closed` and the shim was confirmed gone every time.
			//
			// Kept even though the shim no longer signals its exit before replying, because a shim outlives
			// the server that spawned it: after an upgrade this server still talks to shims from the old
			// build, which do.
			if err := shutdownShim(ctx, f.Socket); err != nil && !isTransportClosed(err) {
				m.log.Warn("shutting down an orphaned shim failed",
					"session", f.Session, "socket", f.Socket, "error", err)
				continue
			}
			// Wait for the socket to stop answering before reporting it stopped, since the shell is
			// signalled before the shim replies and the shim then takes a moment to release the socket.
			// Without this, `cm doctor` run twice in quick succession finds the same shim again, now as a
			// stale socket, which reads as the repair having failed. Bounded rather than open-ended: a shim
			// that will not let go is a real problem, and the report is already accurate about the
			// shutdown having been accepted.
			// Logged rather than returned: it is still reported as stopped, because the shutdown was
			// accepted and the shell signalled before any of this, and a shim that is slow to release its
			// socket has not undone that.
			if err := waitForSocketFree(ctx, f.Socket, shimReleaseTimeout); err != nil {
				m.log.Warn("an orphaned shim was slow to release its socket after shutting down",
					"session", f.Session, "socket", f.Socket, "error", err)
			}
			done = append(done, fmt.Sprintf("stopped orphaned shim for %s (pid %d)", f.Session, f.ShimPID))

		case FindingStaleSocket:
			if err := os.Remove(f.Socket); err != nil && !os.IsNotExist(err) {
				m.log.Warn("removing a stale socket failed", "socket", f.Socket, "error", err)
				continue
			}
			done = append(done, fmt.Sprintf("removed stale socket for %s", f.Session))

		case FindingLooseDirPerms:
			// Safe to do automatically in a way that killing things is not: tightening a mode takes access
			// away from other users and leaves cm's own behavior identical. The path is in Socket, which is
			// where checkDirPerms puts it.
			if err := os.Chmod(f.Socket, 0o700); err != nil {
				m.log.Warn("tightening directory permissions failed", "path", f.Socket, "error", err)
				continue
			}
			done = append(done, fmt.Sprintf("restricted %s to its owner", f.Socket))
		}
	}
	return done
}

// Doctor reports problems and optionally repairs them, for the RPC.
func (s *Service) Doctor(
	ctx context.Context, req *serverv1.DoctorRequest,
) (*serverv1.DoctorResponse, error) {
	findings, err := s.mgr.Diagnose(ctx, req.ClientVersion, capability.Parse(req.GetClientCapabilities()))
	if err != nil {
		return nil, err
	}

	resp := &serverv1.DoctorResponse{
		Findings:      make([]*serverv1.Finding, 0, len(findings)),
		ServerVersion: s.mgr.Version(),
		// So `cm version` can list them from the Doctor call it already makes for the version, rather than
		// making a second call to Status for the same question.
		ServerCapabilities: capability.Server().Strings(),
	}
	if req.Repair {
		resp.Repaired = s.mgr.Repair(ctx, findings)
	}
	for _, f := range findings {
		resp.Findings = append(resp.Findings, &serverv1.Finding{
			Kind:     f.Kind,
			Session:  f.Session,
			Socket:   f.Socket,
			ShimPid:  int32(f.ShimPID),
			ShellPid: int32(f.ShellPID),
			Detail:   f.Detail,
			Fixable:  f.Fixable,
		})
	}
	return resp, nil
}

// shimSockets lists the shim socket files in the runtime directory.
func (m *Manager) shimSockets() ([]string, error) {
	entries, err := os.ReadDir(m.dirs.Runtime)
	if err != nil {
		if os.IsNotExist(err) {
			// No runtime directory means no sessions, which is not a problem to report.
			return nil, nil
		}
		return nil, fmt.Errorf("reading %s: %w", m.dirs.Runtime, err)
	}

	var out []string
	for _, e := range entries {
		name := e.Name()
		if strings.HasPrefix(name, shimSocketPrefix) && strings.HasSuffix(name, shimSocketSuffix) {
			out = append(out, filepath.Join(m.dirs.Runtime, name))
		}
	}
	sort.Strings(out)
	return out, nil
}

// sessionIDFromSocket recovers a session ID from its socket path.
//
// Derived from the path rather than by asking the shim, so a socket nothing answers on still reports which
// session it belonged to. An ID rather than a name because that is what the path holds: a name is a
// binding in the database, and this has to work when the database is the thing that is wrong.
func sessionIDFromSocket(path string) string {
	base := filepath.Base(path)
	base = strings.TrimPrefix(base, shimSocketPrefix)
	return strings.TrimSuffix(base, shimSocketSuffix)
}

// probeShimState asks a shim about itself, reporting whether it answered.
//
// Retries a failed dial, which matters because --clean acts on the answer by *removing* the socket:
// reporting a live shim as stale and then unlinking its socket orphans it, leaving a real shell
// running with a pty on a path nothing can name. A refusal is not evidence of absence, since a unix
// listener refuses once its accept queue fills, with the same errno a socket nobody serves produces.
// A busy listener recovers within about 11ms of returning to its accept loop, so retrying separates
// the two where a single dial cannot.
func probeShimState(ctx context.Context, socket string) (*shimv1.StateResponse, bool) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	deadline := time.Now().Add(socketRefusalGrace)
	for {
		st, ok := tryShimState(ctx, socket)
		if ok {
			return st, true
		}
		if time.Now().After(deadline) || ctx.Err() != nil {
			return nil, false
		}
		select {
		case <-ctx.Done():
			return nil, false
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// tryShimState is one attempt at asking a shim about itself.
func tryShimState(ctx context.Context, socket string) (*shimv1.StateResponse, bool) {
	conn, cl, err := dialShim(socket)
	if err != nil {
		return nil, false
	}
	defer conn.Close()

	st, err := cl.State(ctx, &shimv1.StateRequest{})
	if err != nil {
		return nil, false
	}
	return st, true
}

// shutdownShim asks a shim to stop.
func shutdownShim(ctx context.Context, socket string) error {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	conn, cl, err := dialShim(socket)
	if err != nil {
		return err
	}
	defer conn.Close()

	// Force, because an orphan's shell is not going to be asked politely by anyone else: nothing is
	// attached and nothing will be.
	_, err = cl.Shutdown(ctx, &shimv1.ShutdownRequest{Force: true})
	return err
}

// probeTimeout bounds how long to wait for a shim to answer.
//
// Short, because these are local sockets and the alternative to a timeout is a diagnostic command that
// hangs on the very wedged shim it is meant to report.
const probeTimeout = 2 * time.Second

// Socket naming, kept next to the code that parses it back.
//
// Derived from paths.Dirs.ShimSocket rather than duplicated as literals, so a change to the naming cannot
// silently stop this from finding anything.
var (
	shimSocketPrefix, shimSocketSuffix = shimSocketAffixes()
)

// shimSocketAffixes splits a sample shim socket name into the parts around the session name.
func shimSocketAffixes() (prefix, suffix string) {
	const sample = "\x00session\x00"
	d := paths.Dirs{Runtime: "/"}
	base := filepath.Base(d.ShimSocket(sample))
	parts := strings.SplitN(base, sample, 2)
	if len(parts) != 2 {
		// Unreachable unless ShimSocket stops embedding the name, in which case scanning cannot work and
		// failing loudly beats silently finding nothing.
		panic("cm: shim socket naming no longer embeds the session name")
	}
	return parts[0], parts[1]
}
