package server

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/cmlog"
)

// TestCapabilitySkewIsSilentForAClientOfThisBuild is the regression guard for a check that fired on a
// healthy installation.
//
// The first version compared the client's reported set against capability.Server(), which found
// wait.reported-state and wait.match "missing from the client" and reported skew between a client and
// server built from the same commit. A client has no business declaring a server capability, so the
// comparison was a category error rather than a strictness question.
//
// A role's set is only comparable with the same role's set from another build. That is what makes this
// check quiet, and quiet on healthy installs is the bar: a diagnostic that always fires teaches people to
// ignore diagnostics, which is why shimSkewReportThreshold is three rather than one.
func TestCapabilitySkewIsSilentForAClientOfThisBuild(t *testing.T) {
	mgr := &Manager{log: cmlog.Discard()}

	got := mgr.checkCapabilitySkew(capability.Client())

	if len(got) != 0 {
		t.Errorf("checkCapabilitySkew() = %+v for a client of this same build, want nothing.\n"+
			"This check must compare a client against capability.Client(), not against capability.Server(): "+
			"the two roles legitimately declare different things, so comparing across them reports skew on "+
			"every healthy install.", got)
	}
}

// A client reporting nothing is left to checkVersionSkew, which fires on the same cause.
func TestCapabilitySkewIsSilentForAClientThatReportsNothing(t *testing.T) {
	mgr := &Manager{log: cmlog.Discard()}

	got := mgr.checkCapabilitySkew(capability.Parse(nil))

	if len(got) != 0 {
		t.Errorf("checkCapabilitySkew() = %+v for a client that reports nothing, want nothing: the empty "+
			"version it also sends already produces a version-skew finding, and two findings for one "+
			"restart reads as two problems", got)
	}
}

// TestCapabilitySkewNamesWhichSideIsAhead covers the direction, which is what no version string can give.
//
// Two build hashes cannot be ordered, so "client is X and server is Y" leaves the reader to work out
// which one to replace. A token one side has never heard of settles it.
//
// Only the client-is-newer direction is covered, because only it is reachable. capability.Client() holds
// Reported alone, so any client that reports at all is missing nothing, and the older-client branch cannot
// fire until a second client token exists. Constructing a fake client set to exercise it would test the
// test rather than the check.
func TestCapabilitySkewNamesWhichSideIsAhead(t *testing.T) {
	tests := []struct {
		name string
		// caps is what the client reported.
		caps capability.Set
		want string
	}{
		{
			name: "a client reporting an unknown token is the newer side",
			caps: capability.Parse([]string{string(capability.Reported), "attach.teleport"}),
			want: "the client is the newer side",
		},
		{
			name: "an unknown token alongside known ones still reads normally",
			caps: capability.Parse([]string{
				string(capability.Reported), "attach.teleport", "attach.warp",
			}),
			want: "attach.teleport, attach.warp",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &Manager{log: cmlog.Discard()}

			got := mgr.checkCapabilitySkew(tt.caps)

			if len(got) != 1 {
				t.Fatalf("checkCapabilitySkew() = %+v, want exactly one finding", got)
			}
			if got[0].Kind != FindingCapabilitySkew {
				t.Errorf("finding kind = %q, want %q", got[0].Kind, FindingCapabilitySkew)
			}
			if !strings.Contains(got[0].Detail, tt.want) {
				t.Errorf("detail = %q, want it to say %q", got[0].Detail, tt.want)
			}
		})
	}
}

// TestShimSkewSaysWhatTheSpreadCosts covers the capability clause on the existing shim finding.
//
// Attached to a finding that already fires rather than becoming a finding of its own, because "some shim
// lacks a capability" is true of every shim predating capability reporting and a check that fires on a
// healthy install is noise. What the clause adds is the question a build list cannot answer: what the
// spread actually costs.
func TestShimSkewSaysWhatTheSpreadCosts(t *testing.T) {
	// Three builds, which is what shimSkewReportThreshold requires before anything is reported at all.
	shims := map[string]string{"a": "v1", "b": "v2", "c": "v3"}

	tests := []struct {
		name string
		caps map[string]capability.Set
		want string
	}{
		{
			name: "shims predating capability reporting cannot be asked",
			caps: map[string]capability.Set{
				"a": capability.Parse(nil),
				"b": capability.Parse(nil),
				"c": capability.Shim(),
			},
			want: "2 of them predate capability reporting",
		},
		{
			name: "a shim reporting without a capability is a definite gap",
			caps: map[string]capability.Set{
				"a": capability.Parse([]string{string(capability.Reported)}),
				"b": capability.Shim(),
				"c": capability.Shim(),
			},
			want: "some do not implement " + string(capability.ShutdownSignal),
		},
		{
			name: "a shim reporting an unknown token is newer than this server",
			caps: map[string]capability.Set{
				"a": capability.Parse([]string{string(capability.Reported), "pty.teleport"}),
				"b": capability.Shim(),
				"c": capability.Shim(),
			},
			want: "it is the stale side",
		},
		{
			name: "shims that all agree with this build say nothing extra",
			caps: map[string]capability.Set{
				"a": capability.Shim(),
				"b": capability.Shim(),
				"c": capability.Shim(),
			},
			want: "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			mgr := &Manager{log: cmlog.Discard()}

			got := mgr.checkShimVersionSkew(shims, tt.caps)

			if len(got) != 1 {
				t.Fatalf("checkShimVersionSkew() = %+v, want exactly one finding", got)
			}
			switch {
			case tt.want == "":
				if strings.Contains(got[0].Detail, "On capabilities") {
					t.Errorf("detail = %q, want no capability clause: every shim agrees with this build, so "+
						"the spread of builds has no consequence to report", got[0].Detail)
				}
			case !strings.Contains(got[0].Detail, tt.want):
				t.Errorf("detail = %q, want it to say %q", got[0].Detail, tt.want)
			}
		})
	}
}
