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
	)
}

// Client is what this build's client can do.
//
// Only Reported so far, which is not a placeholder: a client's set exists so a server can *report* the
// skew, and the server decides nothing on it. There is no client capability a server needs to branch on
// yet, and adding one before there is would be the bookkeeping without the benefit.
//
// Worth stating because the asymmetry looks like an omission. A capability probe only ever helps the peer
// that knows a capability exists, and the server is not the peer waiting on the client for anything.
func Client() Set {
	return New(
		Reported,
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
