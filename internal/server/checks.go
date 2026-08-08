package server

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// Additional finding kinds, each encoding a failure that has actually happened here rather than one
// imagined. The point of a diagnostic is to shorten the next debugging session, so the checks are drawn from
// the bugs that took longest to find.
const (
	// FindingVersionSkew is a client and server from different builds.
	FindingVersionSkew = "version-skew"
	// FindingServerErrors is errors in the server's log.
	FindingServerErrors = "server-errors"
	// FindingNoTerminal is a build without the terminal emulator.
	FindingNoTerminal = "no-terminal"
	// FindingLongSocketPath is a runtime directory close to the limit on a unix socket path.
	FindingLongSocketPath = "long-socket-path"
	// FindingNoShellIntegration is a session whose shell never reports OSC 133.
	FindingNoShellIntegration = "no-shell-integration"
	// FindingSessionBacklog is a large number of finished session records.
	FindingSessionBacklog = "session-backlog"
)

// checkVersionSkew reports a client and server built from different sources.
//
// Worth a warning rather than an error, since cm is designed for exactly this: a session outlives its server,
// so an upgrade means a new server adopting sessions a previous one created, and a client may well be a
// different build from the server it reaches.
//
// It is worth reporting because the failure mode is silent. Protobuf ignores unknown fields, so a newer
// client asking an older server for something it does not implement gets a zero value rather than an error.
// `cm wait --until blocked` against a server that predates reporting does not fail; it waits forever. That
// looks like a broken feature rather than a version difference, which is a bad hour of debugging.
func checkVersionSkew(clientVersion string) []Finding {
	server := paths.Version()

	switch {
	case clientVersion == "":
		// A client too old to send its version at all, which is itself the mismatch.
		return []Finding{{
			Kind:    FindingVersionSkew,
			Detail:  fmt.Sprintf("client did not report a version; server is %s", server),
			Fixable: false,
		}}
	case clientVersion != server:
		return []Finding{{
			Kind: FindingVersionSkew,
			Detail: fmt.Sprintf(
				"client is %s and server is %s; a feature missing from one side fails silently rather "+
					"than reporting an error, so restart the server after upgrading",
				clientVersion, server),
			Fixable: false,
		}}
	}
	return nil
}

// maxLoggedErrors bounds how many log lines a finding quotes.
//
// A bound rather than all of them: a server that has been up for weeks can have hundreds, and a diagnostic
// that prints hundreds of lines is one nobody reads. The log itself is available through `cm logs`.
const maxLoggedErrors = 5

// checkServerLog reports errors in the server's log.
//
// The log is where cm records the things it could not do anything about: a terminal model that failed and
// disabled screen restore for a session, a store write that did not land, a session that should have been
// reaped and was not. Every one of those is invisible in normal output by design, because the alternative was
// interrupting the user's terminal, and the result is that nobody ever looks.
func (m *Manager) checkServerLog() []Finding {
	path := m.dirs.ServerLog()
	f, err := os.Open(path)
	if err != nil {
		// No log is not a problem: a server that has just started may not have written one.
		return nil
	}
	defer f.Close()

	var (
		lines []string
		total int
	)
	sc := bufio.NewScanner(f)
	// A long line is possible, since a logged error can embed a command line or a path.
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := sc.Text()
		// Matched on the level rather than on the word "error" anywhere, so a session named "error-repro"
		// or a command containing it does not produce a finding.
		if !strings.Contains(line, "level=ERROR") {
			continue
		}
		total++
		if len(lines) < maxLoggedErrors {
			lines = append(lines, strings.TrimSpace(line))
		}
	}

	if total == 0 {
		return nil
	}
	detail := fmt.Sprintf("%d error(s) in %s", total, path)
	if total > len(lines) {
		detail += fmt.Sprintf(", showing the first %d", len(lines))
	}
	return []Finding{{
		Kind:   FindingServerErrors,
		Detail: detail + ":\n  " + strings.Join(lines, "\n  "),
		// Not fixable: an error in a log is a record of something that already happened, and deleting the
		// log would destroy the evidence rather than fix anything.
		Fixable: false,
	}}
}

// checkTerminal reports a build without the emulator.
//
// A no-cgo build loses screen restore on reattach and `cm history` while everything else works. The server
// logs that once at startup, which is exactly the sort of message nobody sees, and the symptom is a reattach
// showing a blank screen: indistinguishable from a bug in restore.
func (m *Manager) checkTerminal() []Finding {
	if m.newTerminal != nil {
		return nil
	}
	return []Finding{{
		Kind: FindingNoTerminal,
		Detail: "this build has no terminal emulator, so reattaching shows a blank screen and " +
			"`cm history` is unavailable; sessions, attach, detach, and persistence still work",
		Fixable: false,
	}}
}

// socketHeadroom is how many bytes of session name a runtime directory should leave room for.
//
// A session name is the variable part of a socket path. Real ones are short: the kitty integration produces
// kitty.164, and a server-allocated name is s1. 24 bytes is several times that.
//
// Calibrated rather than picked. An earlier value of 40 flagged a working installation whose runtime directory
// was a macOS temp path, which is 66 characters before anything is appended -- a false positive on a setup
// that has never failed, which is the fastest way to teach someone to ignore a diagnostic.
const socketHeadroom = 24

// checkSocketPath reports a runtime directory long enough to threaten the limit on a unix socket path.
//
// This one cost real time: a socket path over the limit fails as a bare EINVAL from bind, with nothing in the
// message about length. It happened here because a test helper embedded the test name in the path, and it
// will happen to anyone whose TMPDIR is deep.
func (m *Manager) checkSocketPath() []Finding {
	// Measured against a plausible name rather than an empty one, so this reports a directory that works
	// today and fails on a longer session name tomorrow.
	sample := m.dirs.ShimSocket(strings.Repeat("n", socketHeadroom))
	if len(sample) <= paths.MaxSocketPathLen {
		return nil
	}
	return []Finding{{
		Kind: FindingLongSocketPath,
		Detail: fmt.Sprintf(
			"runtime directory %s leaves less than %d bytes for a session name before exceeding the "+
				"%d-byte limit on a unix socket path, which fails as an unexplained EINVAL; set %s to "+
				"something shorter",
			m.dirs.Runtime, socketHeadroom, paths.MaxSocketPathLen, paths.Env("RUNTIME_DIR")),
		Fixable: false,
	}}
}

// checkShellIntegration reports live sessions whose shells have never said what they are doing.
//
// cm derives busy and idle from OSC 133, which a shell only sends with terminal integration loaded. Without
// it a session is permanently idle as far as cm can tell, so `cm wait --until idle` returns immediately and
// `cm list` never shows what is running. Nothing errors; the feature just quietly does nothing.
//
// Reported only for sessions that have run at least one command, since a shell sitting at its first prompt
// has had no occasion to report anything and flagging it would be noise.
func (m *Manager) checkShellIntegration() []Finding {
	m.mu.Lock()
	sessions := make([]*Session, 0, len(m.sessions))
	for _, sess := range m.sessions {
		sessions = append(sessions, sess)
	}
	m.mu.Unlock()

	var quiet []string
	for _, sess := range sessions {
		if ended, _ := sess.Ended(); ended {
			continue
		}
		// A session that has reported a command, ever, has working integration. Runs is monotonic, so this
		// stays true after the command finishes.
		if sess.CommandRuns() > 0 {
			continue
		}
		// A report from a program takes the place of shell markers, so a session with one is not affected.
		if sess.Reported().State != "" {
			continue
		}
		quiet = append(quiet, sess.name)
	}

	if len(quiet) == 0 {
		return nil
	}
	return []Finding{{
		Kind: FindingNoShellIntegration,
		Detail: fmt.Sprintf(
			"%d session(s) have never reported a command via OSC 133 (%s), so `cm list` cannot show what "+
				"they are running and `cm wait --until idle` returns immediately; load your terminal's "+
				"shell integration in the session's shell, or report state with `cm report`",
			len(quiet), strings.Join(quiet, ", ")),
		Fixable: false,
	}}
}

// backlogThreshold is how many finished session records count as a backlog.
//
// Chosen from experience rather than arithmetic: `cm list` stops being readable somewhere around a screenful,
// and twenty finished records means the live sessions are already hard to pick out.
const backlogThreshold = 20

// checkSessionBacklog reports a pile of finished session records.
//
// Two things caused this here, both silent. Expiry was gated on persistence being enabled, so a default
// install never cleaned up; and finished `cm run` records shared the week-long lifetime of a deliberately
// persisted session. Either way `cm list` fills with every command ever run and the sessions being worked on
// are lost in it.
func (m *Manager) checkSessionBacklog(ctx context.Context) []Finding {
	records, err := m.store.List(ctx, "")
	if err != nil {
		return nil
	}

	finished := 0
	for _, rec := range records {
		if rec.State != store.StateRunning {
			finished++
		}
	}
	if finished < backlogThreshold {
		return nil
	}

	detail := fmt.Sprintf("%d finished session records are still listed", finished)
	if m.persist == nil || m.persist.ExpireAfter <= 0 {
		detail += ", and expiry is not configured, so they will never be removed"
	} else {
		detail += fmt.Sprintf(", which is expected if they are newer than the %s expiry",
			m.persist.ExpireAfter)
	}
	return []Finding{{
		Kind:   FindingSessionBacklog,
		Detail: detail,
		// Not fixable here. Removing a record is `cm kill`'s job or expiry's, and doing it from a diagnostic
		// would discard a finished command's output that the user may still want to read.
		Fixable: false,
	}}
}
