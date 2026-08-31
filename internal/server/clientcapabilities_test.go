package server

import (
	"reflect"
	"slices"
	"testing"

	"github.com/chancez/cm/internal/capability"
)

// TestAnAttachedClientsCapabilitiesAreReported closes the one hop a client's set could not reach.
//
// Before this, a client reported its capabilities on `cm doctor` and `cm version` alone, so the server
// learned nothing about a client that only ever attached, which is every ordinary client. AttachedClient
// carried a version and nothing about what that version could do, which is the question a build string
// leaves open: the field exists because diagnosing a session loss meant reconstructing what was attached
// from ps and lsof, and a hash still does not say what the thing can do.
func TestAnAttachedClientsCapabilitiesAreReported(t *testing.T) {
	sess, toks := newSessionWithClients(t, "caps", 1)

	sess.noteClientIdentity(toks[0], "v1", 4242, capability.Client())

	got := sess.AttachedClients()
	if len(got) != 1 {
		t.Fatalf("AttachedClients() = %+v, want exactly one", got)
	}
	if want := capability.Client().Names(); !slices.Equal(got[0].Capabilities.Names(), want) {
		t.Errorf("Capabilities = %v, want %v", got[0].Capabilities.Names(), want)
	}
	// The identity fields must survive alongside it, since they share one record and a wrong assignment
	// there would be silent.
	if got[0].PID != 4242 || got[0].Version != "v1" {
		t.Errorf("AttachedClients()[0] = %+v, want pid 4242 and version v1", got[0])
	}
}

// TestAClientThatReportsNoCapabilitiesIsUnknownRatherThanEmpty is the compatibility case.
//
// A client predating the field sends nothing, and the server must hold that as the zero Set so every
// capability reads Unknown. Holding it as a populated-but-empty set would make such a client look like one
// that reports capabilities and supports none of them, which is the distinction the mechanism exists to
// preserve.
func TestAClientThatReportsNoCapabilitiesIsUnknownRatherThanEmpty(t *testing.T) {
	sess, toks := newSessionWithClients(t, "oldcaps", 1)

	// What Service.Attach does with an older client's Open: parse a field that is not there.
	sess.noteClientIdentity(toks[0], "", 0, capability.Parse(nil))

	got := sess.AttachedClients()
	if len(got) != 1 {
		t.Fatalf("AttachedClients() = %+v, want exactly one", got)
	}
	if got[0].Capabilities.Reports() {
		t.Errorf("a client that sent no capabilities reports %v, want a set that answers unknown",
			got[0].Capabilities)
	}
	answers := make(map[capability.Name]capability.Support)
	want := make(map[capability.Name]capability.Support)
	for _, n := range capability.Declared() {
		answers[n] = got[0].Capabilities.Supports(n)
		want[n] = capability.Unknown
	}
	if !reflect.DeepEqual(answers, want) {
		t.Errorf("a client that sent no capabilities answers %v, want everything unknown", answers)
	}
}
