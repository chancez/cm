package server

import (
	"context"
	"fmt"
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
	// FindingNoTerminal is a server running without a terminal emulator, which no ordinary build produces
	// now that cgo is required.
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
func (m *Manager) checkVersionSkew(clientVersion string) []Finding {
	server := m.Version()

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

// checkTerminal reports a build without the emulator.
//
// Kept after cgo became mandatory, because a Manager can still be constructed without a terminal factory and
// the symptom is a reattach showing a blank screen, which is indistinguishable from a bug in restore. It should
// no longer fire on a real installation, so if it does, something is wrong rather than merely unsupported.
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
