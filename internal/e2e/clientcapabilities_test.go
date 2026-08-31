package e2e

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/chancez/cm/internal/capability"
)

// clientRow is the part of `cm clients list --json` this test reads.
type clientRow struct {
	Session      string   `json:"session"`
	PID          int32    `json:"pid"`
	Version      string   `json:"version"`
	Capabilities []string `json:"capabilities"`
}

// TestAnAttachedClientReportsItsCapabilities covers the producing side of the client hop.
//
// It has to be end to end, and that is a lesson rather than a preference. The equivalent shim-side field was
// added with server tests that all drove a *fake* shim, so deleting the line that put capabilities on the
// wire passed the entire suite: a consumer test proves nothing about a producer it stubs out. The same shape
// applies here. TestAttachRecordsClientIdentity drives svc.Attach with an Open the test wrote itself, so it
// covers the server's wiring and cannot see internal/client stop sending.
//
// So this runs a real `cm attach` against a real server and reads what the CLI reports, which is the only
// arrangement where the client is the thing under test.
func TestAnAttachedClientReportsItsCapabilities(t *testing.T) {
	skipIfShort(t)
	e := newEnv(t)

	c := attachOnPty(t, e, "capreport", "--", "/bin/sh")
	c.waitReady()

	out := e.mustRun("clients", "list", "capreport", "--json")
	var rows []clientRow
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("parsing `cm clients list --json`: %v\noutput: %s", err, out)
	}
	if len(rows) != 1 {
		t.Fatalf("clients list = %+v, want exactly one attached client", rows)
	}

	// Compared against this build's client set rather than a hardcoded list, so a token added later is
	// carried here without editing the test. What is pinned is that the wire agrees with the registry.
	want := capability.Client().Strings()
	if !slices.Equal(rows[0].Capabilities, want) {
		t.Errorf("an attached client reports capabilities %v, want %v.\n"+
			"Empty means internal/client is not putting them in the Open it sends, or the server is not "+
			"recording them: either way a listing cannot say what an attached client can do, which is the "+
			"whole point of the field.", rows[0].Capabilities, want)
	}
	// The token that makes an empty list conclusive. Without it a server cannot tell this client from one
	// that predates capability reporting, so every capability reads unknown forever.
	if got := capability.Parse(rows[0].Capabilities); !got.Reports() {
		t.Errorf("the reported set %v does not include %q, so it is indistinguishable from a client that "+
			"reports nothing", got, capability.Reported)
	}
}
