package server

import (
	"strings"
	"testing"
)

// Shim build spread is reported only once it is worth reporting.
//
// The threshold is the whole point of this check rather than an implementation detail. A shim keeps its
// pty across every server restart and upgrade, so a session predating an upgrade legitimately runs an
// older build than the server managing it. Firing on the first difference would flag every install that
// has ever been upgraded, and a check that fires on healthy installs is how people learn to ignore
// diagnostics. So the healthy cases are asserted as *silent*, which is the half most likely to regress.
func TestCheckShimVersionSkew(t *testing.T) {
	for _, tc := range []struct {
		name  string
		shims map[string]string
		want  bool
	}{
		{name: "no shims", shims: map[string]string{}, want: false},
		{
			name:  "every shim on one build",
			shims: map[string]string{"a": "v1", "b": "v1", "c": "v1"},
			want:  false,
		},
		{
			// The ordinary shape of an install that was upgraded while sessions were running. Two builds
			// is not a problem and must stay quiet.
			name:  "one upgrade, some sessions predate it",
			shims: map[string]string{"a": "v1", "b": "v2", "c": "v2"},
			want:  false,
		},
		{
			name:  "three builds",
			shims: map[string]string{"a": "v1", "b": "v2", "c": "v3"},
			want:  true,
		},
		{
			// A shim too old to report its version is its own bucket rather than evidence of agreement,
			// so this is three distinct builds and not two.
			name:  "an unreported version counts as its own build",
			shims: map[string]string{"a": "v1", "b": "v2", "c": ""},
			want:  true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr, _, _ := newTestManager(t, nil)
			mgr.SetVersion("v-server")

			got := mgr.checkShimVersionSkew(tc.shims)
			if (len(got) > 0) != tc.want {
				t.Errorf("checkShimVersionSkew(%v) = %+v, want a finding = %v", tc.shims, got, tc.want)
			}
			if len(got) == 0 {
				return
			}
			if got[0].Kind != FindingShimVersionSkew {
				t.Errorf("kind = %q, want %q", got[0].Kind, FindingShimVersionSkew)
			}
			// Never fixable. Restarting the server adopts the same shims and changes nothing here; only
			// ending a session replaces its shim, which costs the shell running in it. Offering to repair
			// this would be offering to kill someone's work.
			if got[0].Fixable {
				t.Error("finding is Fixable, but nothing can fix this without ending a session")
			}
			// The server's own build is named, since "the shims disagree" does not say which side of the
			// difference the reader is standing on.
			if !strings.Contains(got[0].Detail, "v-server") {
				t.Errorf("detail does not name the server build: %q", got[0].Detail)
			}
		})
	}
}

// The detail must name each build and how many sessions run it, in a stable order.
//
// Deterministic because a diagnostic gets diffed between runs and pasted into issues. Map iteration is
// randomized in Go, so building the string without sorting produces a different message each call for
// an unchanged install, which reads as something having changed.
func TestCheckShimVersionSkewDetailIsStable(t *testing.T) {
	mgr, _, _ := newTestManager(t, nil)
	mgr.SetVersion("v-server")

	shims := map[string]string{
		"a": "v2", "b": "v2", "c": "v2",
		"d": "v1", "e": "v1",
		"f": "",
	}

	got := mgr.checkShimVersionSkew(shims)
	if len(got) != 1 {
		t.Fatalf("checkShimVersionSkew() = %+v, want exactly one finding", got)
	}

	// Ordered by count descending, so the build most sessions run is named first, and an unreported
	// version is spelled out rather than left as an empty string in the middle of a sentence.
	if !strings.Contains(got[0].Detail, "v2 (3), v1 (2), unknown (1)") {
		t.Errorf("detail does not list builds by descending count: %q", got[0].Detail)
	}
	// The session count is the total, not the number of builds.
	if !strings.Contains(got[0].Detail, "6 sessions are running 3 different builds") {
		t.Errorf("detail does not report 6 sessions across 3 builds: %q", got[0].Detail)
	}

	// Repeated calls agree. One call cannot catch an ordering bug that depends on map iteration, so this
	// runs enough times that a randomized order would almost certainly differ at least once.
	for range 20 {
		again := mgr.checkShimVersionSkew(shims)
		if len(again) != 1 || again[0].Detail != got[0].Detail {
			t.Fatalf("detail changed between identical calls:\n%q\n%q", got[0].Detail, again[0].Detail)
		}
	}
}
