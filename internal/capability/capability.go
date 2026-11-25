// Package capability names the things one cm process can tell another it is able to do.
//
// cm runs three roles that are routinely different builds. A shim holds a pty across every server
// restart and upgrade, so a new server talking to a ten-day-old shim is the ordinary state rather than
// a broken one, and replacing the binary is enough to produce it: a shim is re-exec'd from disk, so
// installing a build and carrying on working pairs an old server with new shims with no upgrade command
// involved.
//
// The compat rules that follow from that are stated in shim.proto's header, and they leave one thing
// out: they tell a peer to treat absence as "predates this" without giving it any way to tell absence
// from a peer that means no. Protobuf reads a field nobody sent as its zero value, so an old peer looks
// exactly like a peer that answered false. That silence has cost real time here. `cm wait --until
// blocked` against a server predating state reporting waits forever, because nothing on the wire
// separates "not blocked yet" from "will never report blocked".
//
// A capability set is those rules made checkable: a peer says what it can do, and a caller asks before
// depending on it.
//
// This package is vocabulary only. It holds no wire types, no transport, and no policy about what to do
// when something is missing, which is why every role can import it and why a token can be checked in a
// test without standing up a peer.
package capability

import (
	"sort"
	"strings"
)

// Name is one capability token.
//
// A distinct type rather than a string, so a gate cannot be written against a literal nobody declared.
// The wire carries plain strings, and the conversion is confined to Parse and Strings below.
type Name string

// The declared capabilities. Each names something with a consequence when it is missing, same bar as a
// `cm doctor` check: a token for a hypothetical is noise, and noise teaches people to ignore the
// mechanism.
//
// Two rules govern this list, both learned from proto fields rather than guessed at:
//
//   - A token is added by the commit that adds the behavior. Out of order, the set claims something the
//     build cannot do, which is worse than the silence it replaces: a missing field fails at the field,
//     while a lying token makes a caller commit to a path that cannot work.
//   - A token is never removed or renamed, for the same reason a proto field is not.
const (
	// Reported means the peer reports capabilities at all.
	//
	// Every role declares it, which is what makes an empty list conclusive: a peer that speaks this
	// mechanism always sends at least one token, so nothing back means the peer predates the mechanism
	// rather than that it can do nothing. Without it, "I have no capabilities" and "I have never heard
	// of capabilities" are the same message, and the whole point here is to stop reading one as the
	// other.
	Reported Name = "capabilities"

	// ShutdownSignal means the shim honors ShutdownRequest.signal rather than deriving the signal from
	// the force flag alone.
	//
	// A shim without it sends SIGHUP, or SIGKILL when forced, whatever `cm kill --signal` asked for. The
	// shell dies either way, so this degrades rather than fails, and what was wrong was that nothing
	// said so: shim.proto claimed the server "logs it rather than pretending the signal was delivered"
	// and no such log existed. A job deliberately trapping SIGTERM to finish writing a file is the case
	// that cares.
	ShutdownSignal Name = "shutdown.signal"

	// WaitReportedState means the server understands Report and ReportedState, so a wait for the blocked
	// state can be satisfied.
	//
	// The failure without it is a hang rather than an error, which is the whole reason this package
	// exists. `cm wait --until blocked` against a server predating reporting waits out its timeout,
	// because nothing on the wire separates "not blocked yet" from "will never report blocked". Recorded
	// in checks.go as costing a bad hour, since it looks like a broken feature.
	WaitReportedState Name = "wait.reported-state"

	// WaitMatch means the server honors WaitRequest.match, so a wait for text can be satisfied.
	//
	// The same hang one field over, and worse in one respect: an older server reads the unknown field as
	// absent and sees a wait with no condition at all, so it does not even fail loudly.
	WaitMatch Name = "wait.match"

	// WaitProgramStatus means the server knows the program status protocol's done and error states, so a
	// wait for either can be satisfied.
	//
	// The same hang as WaitReportedState, for the two states that arrived with OSC 7501: an older server
	// reads the state as one it does not know and never considers the wait satisfied.
	WaitProgramStatus Name = "wait.program-status"

	// KillExpireAfter means the server honors KillRequest.expire_after, giving a kept session its own
	// retention.
	//
	// Without it the failure is silent and a week late: an older server reads the field as absent, keeps
	// the session on the configured schedule, and reports success. A session suspended to be revived later
	// then loses its record, its saved output and its resume command when expire_after passes.
	KillExpireAfter Name = "kill.expire-after"

	// Shelve means the server implements the Shelve and Restore RPCs, the shelved state, and
	// Open.cwd_explicit for a restore.
	//
	// Checked before `cm shelve` and `cm restore` send anything. An older server answers the RPCs with
	// an Unimplemented error whose text names a method rather than the cause, and the remedy, restarting
	// the server onto the binary already installed, appears nowhere in it.
	Shelve Name = "shelve"

	// Spaces means the server implements the Spaces, CreateSpace, RemoveSpace and Move RPCs, Open.space,
	// ListRequest.space and Session.space.
	//
	// Checked before anything that names a space is sent, except an attach. The failure it prevents is
	// silent rather than an error: an older server reads ListRequest.space as absent, so `cm list --space
	// infra` lists every session. An attach would need a Status call of its own, since the attach
	// handshake reports no capabilities, and an attach is every window's startup; against an older server
	// `cm attach --space infra` creates a session the server files in no space, which `cm space` then
	// reports by refusing.
	Spaces Name = "spaces"

	// Close means the server implements the Close RPC, which applies the window-close policy.
	//
	// Checked before `cm close` sends anything, for the reason Shelve is: an older server answers with an
	// Unimplemented error naming a method, and a close watcher that reads it as a failure leaves the window's
	// session as it was, which is at least not destructive.
	Close Name = "close"

	// SpaceMeta means the server implements the ListSpaceMeta, GetSpaceMeta, SetSpaceMeta and
	// RemoveSpaceMeta RPCs, a space's opaque metadata.
	//
	// Checked before `cm space meta` sends anything. A tool saving a layout through it runs unattended, and
	// an older server's Unimplemented error names a method rather than saying the server needs restarting
	// onto the binary already installed.
	SpaceMeta Name = "spaces.meta"

	// SwitchRemote is declared by both roles, for the two halves of moving a window to another host.
	//
	// From a server: Switch accepts a target qualified with another host, and a name can point there. Checked
	// before `cm switch` or `cm rebind` sends one, since an older server would resolve `white:work` as a local
	// name and refuse it with a message about the name rather than the server.
	//
	// From a client: told to switch to a qualified reference, it connects to that host rather than asking
	// this server for it. Checked by the server before it asks a window to move there, because a client that
	// predates this tries the reference here, fails, and the window closes.
	SwitchRemote Name = "switch.remote"

	// HistoryPage means the server implements the HistoryPage RPC, a session's contents a page at a time.
	//
	// Checked before `cm grep` reads anything, which falls back to one whole History against an older
	// server: that works for the scrollback most sessions hold, and the client has no other way to read one.
	HistoryPage Name = "history.page"
)

// Support is what a caller learns when it asks about one capability.
//
// Three answers rather than two, and that is the point of the type. A bool would have to fold "this peer
// says no" together with "this peer told me nothing", and those call for opposite behavior: the first is
// a fact to act on, the second is a reason to act as cm always did. cm has been here before with
// sockets, where ECONNREFUSED means both "nobody is listening" and "a live listener's queue is full",
// and only ENOENT is conclusive. Same shape, same rule: one of the three answers is the absence of
// information, not a value.
type Support int

const (
	// Unknown means the peer reports no capabilities, so nothing can be concluded about any of them.
	//
	// The zero value deliberately. A Set nobody filled in answers Unknown to everything, so code that
	// forgets to populate one degrades to today's behavior rather than to a confident wrong answer.
	Unknown Support = iota
	// Absent means the peer reports capabilities and this is not among them.
	Absent
	// Present means the peer reports this capability.
	Present
)

func (s Support) String() string {
	switch s {
	case Present:
		return "present"
	case Absent:
		return "absent"
	default:
		return "unknown"
	}
}

// Set is what one peer reports about itself.
//
// The zero value is a peer that reported nothing, which is the state a server holds about a shim it has
// not asked yet as well as about one too old to answer. Both mean the same thing to a caller, which is
// why they are not distinguished.
type Set struct {
	// have is nil for a peer that reported nothing. Non-nil and containing Reported for any peer that
	// speaks this mechanism, which is what Supports relies on to answer Absent rather than Unknown.
	have map[Name]struct{}
}

// New builds the set a role declares about itself.
func New(names ...Name) Set {
	have := make(map[Name]struct{}, len(names))
	for _, n := range names {
		have[n] = struct{}{}
	}
	return Set{have: have}
}

// Parse builds a set from what a peer sent.
//
// Unrecognized tokens are kept rather than dropped, and that is load-bearing rather than tidiness. A
// token this build has never heard of means the peer is *newer*, which is a direction of skew nothing in
// cm can currently detect at all: a version string differs without saying which side is ahead. See
// Unrecognized.
func Parse(tokens []string) Set {
	if len(tokens) == 0 {
		// Distinct from a set holding nothing, since Supports answers Unknown for one and Absent for the
		// other. An empty slice on the wire is what an older peer sends.
		return Set{}
	}
	have := make(map[Name]struct{}, len(tokens))
	for _, t := range tokens {
		have[Name(t)] = struct{}{}
	}
	return Set{have: have}
}

// Supports reports what is known about one capability.
//
// There is deliberately no Has: a bool here is the mistake this type exists to prevent, because a
// caller that cannot see the difference between Absent and Unknown will treat every shim predating the
// mechanism as one that refused.
func (s Set) Supports(n Name) Support {
	if !s.Reports() {
		return Unknown
	}
	if _, ok := s.have[n]; ok {
		return Present
	}
	return Absent
}

// Reports says whether this peer speaks the capability mechanism at all.
func (s Set) Reports() bool {
	_, ok := s.have[Reported]
	return ok
}

// Missing returns the wanted capabilities this peer does not report, sorted.
//
// For a diagnostic naming what a peer cannot do. It counts Unknown as missing, since a diagnostic's job
// is to say what cannot be relied on, and an unreportable capability cannot be.
func (s Set) Missing(want ...Name) []Name {
	var missing []Name
	for _, n := range want {
		if s.Supports(n) != Present {
			missing = append(missing, n)
		}
	}
	sort.Slice(missing, func(i, j int) bool { return missing[i] < missing[j] })
	return missing
}

// Unrecognized returns the tokens this peer reports that this build does not declare, sorted.
//
// Evidence that the peer is newer than this build, which is worth surfacing because the reader's next
// move differs: a peer that is behind is fixed by restarting it, and a peer that is ahead means this
// binary is the stale one. Nothing else cm reports distinguishes those.
func (s Set) Unrecognized() []Name {
	var unknown []Name
	for n := range s.have {
		if _, declared := declared[n]; !declared {
			unknown = append(unknown, n)
		}
	}
	sort.Slice(unknown, func(i, j int) bool { return unknown[i] < unknown[j] })
	return unknown
}

// Names returns everything in the set, sorted.
func (s Set) Names() []Name {
	names := make([]Name, 0, len(s.have))
	for n := range s.have {
		names = append(names, n)
	}
	sort.Slice(names, func(i, j int) bool { return names[i] < names[j] })
	return names
}

// Strings returns the set for the wire, sorted.
//
// Sorted so the same build always sends the same bytes. Map order would otherwise reshuffle a field that
// ends up in logs and diagnostics people diff.
func (s Set) Strings() []string {
	names := s.Names()
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = string(n)
	}
	return out
}

// String renders the set for a log line.
func (s Set) String() string {
	if !s.Reports() {
		return "none reported"
	}
	return strings.Join(s.Strings(), " ")
}

// Shim is what this build's shim can do.
//
// A literal because cm is one binary: a role's capabilities are a property of the build rather than
// something to discover at runtime.
func Shim() Set {
	return New(
		Reported,
		ShutdownSignal,
	)
}

// Server is what this build's server can do.
func Server() Set {
	return New(
		Reported,
		WaitReportedState,
		WaitMatch,
		WaitProgramStatus,
		KillExpireAfter,
		Shelve,
		Spaces,
		Close,
		SpaceMeta,
		SwitchRemote,
		HistoryPage,
	)
}

// Client is what this build's client can do.
//
// Mostly for a server to *report* skew. SwitchRemote is the one a server branches on: it is the server that
// asks a window to move to another host, so it is the peer that has to know whether the client can.
func Client() Set {
	return New(
		Reported,
		SwitchRemote,
	)
}

// declared is every token this build knows, for Unrecognized.
//
// Derived from the role sets rather than listed again, so a token added to a role cannot be left out of
// here and read back as a stranger.
var declared = func() map[Name]struct{} {
	all := make(map[Name]struct{})
	for _, s := range []Set{Shim(), Server(), Client()} {
		for n := range s.have {
			all[n] = struct{}{}
		}
	}
	return all
}()

// Declared returns every capability this build knows about, sorted. For tests and diagnostics.
func Declared() []Name {
	return Set{have: declared}.Names()
}
