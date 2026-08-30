package server

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/chancez/cm/internal/capability"
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
	// FindingShimVersionSkew is running shims spanning several builds. Expected rather than broken, and
	// reported because the consequences of a version difference are silent.
	FindingShimVersionSkew = "shim-version-skew"
	// FindingCapabilitySkew is a client and server that disagree about what one of them can do. Names the
	// feature rather than the build, which is what version-skew cannot do.
	FindingCapabilitySkew = "capability-skew"
)

// checkCapabilitySkew reports what a client and server disagree about being able to do.
//
// This is what checkVersionSkew cannot say. Two different build strings tell a reader that skew exists and
// nothing about what breaks, so the next step is guesswork; this names the feature. `cm wait --until
// blocked` against a server predating state reporting is the case it exists for, because that one hangs
// rather than failing and so looks like a broken feature rather than an old server.
//
// It also reports the direction, which nothing else here can. A token the client sends that this server
// has never heard of means the *client* is newer, so the remedy is to restart the server onto the new
// build rather than to wonder why a client is misbehaving. Version strings are opaque: neither side can
// order two hashes.
//
// Quiet on a healthy install by construction. A client and server from one build declare identical sets,
// so there is nothing to report until they genuinely differ.
func (m *Manager) checkCapabilitySkew(clientCaps capability.Set) []Finding {
	// Compared against what this build's *client* declares, never against what its server declares.
	//
	// That distinction is not pedantry, it is the difference between this check being quiet and it firing
	// on every healthy install. A client and a server are different roles with legitimately different
	// capabilities: wait.reported-state is a thing a server implements and a client has no business
	// declaring. Comparing a client's set against Server() found both wait tokens "missing from the
	// client" and reported skew against a client and server from the same commit, which is exactly the
	// noise shimSkewReportThreshold exists to avoid. Caught by TestDiagnoseFindsNothingWhenHealthy.
	//
	// A role's set is only ever comparable with the same role's set from another build.
	expected := capability.Client()

	// A client that reports nothing is already covered by checkVersionSkew, which fires on the empty
	// version the same client sends. Repeating it here as a second finding about one cause would be noise,
	// and two findings for one restart reads as two problems.
	if !clientCaps.Reports() {
		return nil
	}

	var parts []string
	// What the client knows about and this server does not implement. The failing direction: the client is
	// the newer side and may ask for something that silently never happens.
	if ahead := clientCaps.Unrecognized(); len(ahead) > 0 {
		parts = append(parts, fmt.Sprintf(
			"the client reports %s, which this server does not implement, so the client is the newer side "+
				"and restarting the server will pick the new build up", capabilityList(ahead)))
	}
	// The reverse: this server implements something the client is too old to ask for. Harmless, since a
	// client that cannot ask cannot be disappointed, but it does say which side is stale.
	//
	// Not reachable yet, and kept rather than added later on purpose. capability.Client() holds Reported
	// alone, so any client that reports at all is missing nothing from it; this fires from the first real
	// client capability onwards. What it is doing here now is holding the *correct* comparison in place,
	// against Client() rather than Server(), which is the mistake that made this check fire on every
	// healthy install once already.
	if behind := clientCaps.Missing(expected.Names()...); len(behind) > 0 {
		parts = append(parts, fmt.Sprintf(
			"this server implements %s and the client does not report it, so the client is the older side",
			capabilityList(behind)))
	}
	if len(parts) == 0 {
		return nil
	}

	return []Finding{{
		Kind: FindingCapabilitySkew,
		Detail: strings.Join(parts, "; ") +
			". A capability missing from one side does not error, it reads as a zero value, so the symptom " +
			"is a feature that appears not to work rather than a version difference",
		Fixable: false,
	}}
}

// capabilityList renders capability tokens for a finding's detail.
//
// Not called `names`: this package's names.go is about session names, and two functions called names in
// one package invites reading one for the other. Local variables called names already shadow it in
// checkShimVersionSkew.
func capabilityList(caps []capability.Name) string {
	out := make([]string, len(caps))
	for i, n := range caps {
		out[i] = string(n)
	}
	return strings.Join(out, ", ")
}

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

// shimSkewReportThreshold is how many distinct shim builds are worth mentioning.
//
// Not one, and that is the whole design of this check. A shim outlives the binary that spawned it by
// design: it holds a pty across every server restart and upgrade, so a session started before an
// upgrade legitimately runs an older build than the server managing it. Reporting the first difference
// would fire on every healthy install that has ever been upgraded, and a diagnostic that fires on
// healthy installs teaches people to ignore diagnostics.
//
// Three, calibrated against a real install rather than picked. A machine with a session per terminal
// window and a few upgrades a week sat at twelve builds across twenty-six shims, which is where
// identifying "which client is which" stopped being possible by hand. Two builds is the ordinary shape
// of "upgraded, some sessions predate it" and says nothing. Three or more means upgrades are
// accumulating faster than sessions turn over, which is the state where a silently missing feature
// becomes hard to attribute.
const shimSkewReportThreshold = 3

// checkShimVersionSkew reports how many distinct builds the running shims span.
//
// Informational rather than a fault, for the reason above: this is a designed-for condition, not a
// broken one. It is worth surfacing because the *consequence* is silent. Protobuf reads a field a peer
// never sent as its zero value rather than as an error, so a server asking an old shim for something it
// does not implement gets a plausible-looking answer instead of a failure. That is the same silence
// checkVersionSkew exists for, one hop further away and harder to see, since nothing about a session
// says which build is holding its pty.
//
// The remedy is deliberately not stated as "restart the server". Restarting adopts the same shims and
// changes none of this; only ending a session and starting a new one replaces its shim, which costs the
// shell. So the finding says what is true and leaves the trade to the reader.
func (m *Manager) checkShimVersionSkew(
	shimVersions map[string]string, shimCaps map[string]capability.Set,
) []Finding {
	if len(shimVersions) == 0 {
		return nil
	}

	// Counted by build so the detail can name the spread rather than just its size. An unreported version
	// is its own bucket: a shim too old to answer is not evidence of agreement with anything.
	const unknown = "unknown"
	counts := make(map[string]int, len(shimVersions))
	for _, v := range shimVersions {
		if v == "" {
			v = unknown
		}
		counts[v]++
	}
	if len(counts) < shimSkewReportThreshold {
		return nil
	}

	// Sorted by count and then by name, so the same installation always produces the same string. Map
	// iteration order would otherwise reshuffle the detail between runs of a diagnostic people diff.
	builds := make([]string, 0, len(counts))
	for v := range counts {
		builds = append(builds, v)
	}
	sort.Slice(builds, func(i, j int) bool {
		if counts[builds[i]] != counts[builds[j]] {
			return counts[builds[i]] > counts[builds[j]]
		}
		return builds[i] < builds[j]
	})

	var b strings.Builder
	for i, v := range builds {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%s (%d)", v, counts[v])
	}

	// Named here rather than in a check of its own, and that placement is the decision. A finding for
	// "some shim lacks a capability" would fire on every shim predating capability reporting, which today
	// is all of them, and a diagnostic that fires on a healthy install teaches people to ignore
	// diagnostics -- the same reason shimSkewReportThreshold is three rather than one. Reported only where
	// something already justified a finding, and only to say *what* the spread costs, which is the
	// question a build list cannot answer.
	return []Finding{{
		Kind: FindingShimVersionSkew,
		Detail: fmt.Sprintf(
			"%d sessions are running %d different builds: %s. This is expected, since a shim keeps its "+
				"pty across restarts and so outlives the build that started it, and a restart adopts the "+
				"same shims rather than replacing them. It is reported because the effect is silent: a "+
				"feature one side does not implement reads as a zero value rather than an error. Only "+
				"ending a session replaces its shim, which costs the shell in it, so this is a trade "+
				"rather than a repair. Server is %s%s",
			len(shimVersions), len(counts), b.String(), m.Version(),
			shimCapabilityNote(shimCaps)),
		Fixable: false,
	}}
}

// shimCapabilityNote describes what the shims cannot be relied on for, as a clause to append.
//
// Empty when every shim reports the same capabilities this build's shim has, which is the case worth
// keeping quiet: a spread of builds that all implement the same things is a version difference with no
// consequence, and saying so at length would bury the cases that have one.
func shimCapabilityNote(shimCaps map[string]capability.Set) string {
	want := capability.Shim().Names()

	// Counted three ways because the three call for different words. A shim that reports nothing cannot be
	// asked; one that reports and lacks something is a definite gap; one that reports something this build
	// has never heard of is newer than the server managing it, which is the direction nothing else here can
	// detect.
	silent := 0
	lacking := make(map[capability.Name]int)
	ahead := 0
	for _, caps := range shimCaps {
		if !caps.Reports() {
			silent++
			continue
		}
		for _, n := range caps.Missing(want...) {
			lacking[n]++
		}
		if len(caps.Unrecognized()) > 0 {
			ahead++
		}
	}

	var parts []string
	if silent > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d of them predate capability reporting, so what they implement cannot be established",
			silent))
	}
	if len(lacking) > 0 {
		names := make([]string, 0, len(lacking))
		for n := range lacking {
			names = append(names, fmt.Sprintf("%s (%d)", n, lacking[n]))
		}
		sort.Strings(names)
		parts = append(parts, "some do not implement "+strings.Join(names, ", "))
	}
	if ahead > 0 {
		parts = append(parts, fmt.Sprintf(
			"%d report a capability this build does not know, so they are newer than this server and it is "+
				"the stale side", ahead))
	}
	if len(parts) == 0 {
		return ""
	}
	return ". On capabilities: " + strings.Join(parts, "; ")
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
		// By label, so a reader is told the name they use rather than an ID they have never seen. A
		// session with no name still reports its ID, which is what they would have to type.
		quiet = append(quiet, sess.Label())
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
	records, err := m.store.List(ctx)
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
