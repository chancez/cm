package shim

import (
	"context"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/capability"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// TestAShimReportsWhatItCanDo covers the producing half of capability reporting.
//
// Needed as its own test because every server-side test drives a fake shim, so none of them can see this
// side stop sending. Verified by mutation: deleting the Capabilities line from Service.State left the
// whole suite green, which is the shape of a foundation that reports nothing while looking wired up.
//
// Asserted against capability.Shim() rather than a hardcoded list, so a token added to the shim's set is
// carried here without editing the test. What the test pins is that the wire agrees with the registry,
// which is the only thing a server can rely on.
func TestAShimReportsWhatItCanDo(t *testing.T) {
	cl, _ := startShim(t, Config{
		Session: "captest",
		Command: []string{"/bin/sh", "-c", "sleep 30"},
		Rows:    24, Cols: 80,
	})

	st, err := cl.State(context.Background(), &shimv1.StateRequest{})
	if err != nil {
		t.Fatalf("State() error = %v", err)
	}

	if want := capability.Shim().Strings(); !reflect.DeepEqual(st.Capabilities, want) {
		t.Errorf("State().Capabilities = %v, want %v", st.Capabilities, want)
	}
	// The token that makes an empty list conclusive. Without it a server cannot tell this shim from one
	// that predates the mechanism, so every capability reads as unknown forever and the reporting above
	// buys nothing.
	if got := capability.Parse(st.Capabilities); !got.Reports() {
		t.Errorf("the reported set %v does not include %q, so a server cannot tell it from a shim that "+
			"reports nothing", got, capability.Reported)
	}
}
