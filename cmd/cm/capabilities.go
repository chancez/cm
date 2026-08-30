package main

import (
	"context"
	"fmt"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// serverCapabilities asks a server what it can do.
//
// One unary call, and only made by a command that depends on a server behavior. Measured at 17.8us for a
// unary round trip against the ~23ms any cm invocation costs, so the cost is noise where it is paid and
// nothing where it is not.
//
// Every failure returns the zero Set, which answers capability.Unknown to everything rather than Absent.
// That distinction is the whole point: a server too old to know Status, or one that cannot be reached, has
// told the caller nothing, and treating silence as refusal would break every command against the servers
// that are actually running today.
func serverCapabilities(ctx context.Context, cl serverv1.ServerClient) capability.Set {
	resp, err := cl.Status(ctx, &serverv1.StatusRequest{})
	if err != nil {
		return capability.Set{}
	}
	return capability.Parse(resp.GetCapabilities())
}

// requireServerCapability refuses a command up front when the server has said it cannot do the thing.
//
// Only on capability.Absent, which is a conclusive no. capability.Unknown proceeds, and that asymmetry is
// deliberate rather than timid: a server predating capability reporting is every server running right now,
// so refusing on silence would turn a working command into a failing one on the strength of not having
// asked in time. What Unknown gets instead is explainUnsatisfied below, attached to the failure rather
// than to the attempt.
//
// The error names the remedy, because "unsupported" without it leaves the reader to guess whether the fix
// is a flag, an upgrade, or a restart. It is a restart: the binary on disk is already new, and the server
// is the process still running the old one.
func requireServerCapability(caps capability.Set, want capability.Name, what string) error {
	if caps.Supports(want) != capability.Absent {
		return nil
	}
	return fmt.Errorf(
		"the running server cannot %s: it does not implement %q.\n"+
			"Restart it with `%s server restart` to pick up this binary. Sessions survive a restart, since "+
			"each one is held by its own shim rather than by the server",
		what, want, paths.Name)
}

// explainUnsatisfied returns a sentence to append when something was not satisfied and a capability it
// needed could not be confirmed.
//
// This is where the Unknown case earns its keep. The failure being addressed is a wait that runs its whole
// timeout against a server that could never have satisfied it, which reads as a broken feature rather than
// as an old server, and checks.go records that costing a bad hour. Naming the doubt at the moment of
// failure costs nothing on the path where the wait succeeds.
//
// Empty when the capability is present, so a genuine timeout is not muddied by a compatibility note that
// does not apply.
func explainUnsatisfied(caps capability.Set, want capability.Name) string {
	switch caps.Supports(want) {
	case capability.Unknown:
		return fmt.Sprintf(
			"\nThe server did not report its capabilities, so it may predate %q and may never have been "+
				"able to satisfy this. `%s version` shows both builds; a restart picks up this one.",
			want, paths.Name)
	case capability.Absent:
		// Reachable only where a caller chose to warn rather than refuse. Stated plainly, since at this
		// point it is a fact rather than a possibility.
		return fmt.Sprintf(
			"\nThe server does not implement %q, so this could not have been satisfied. Restart it with "+
				"`%s server restart`.", want, paths.Name)
	}
	return ""
}
