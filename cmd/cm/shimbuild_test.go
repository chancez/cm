package main

import (
	"bytes"
	"strings"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// An unknown shim build says so rather than printing nothing.
//
// The distinction is the whole reason the field exists. A blank value reads as "same as the server", and
// the server's build is exactly what a long-lived session's shim is *not*: only ending a session replaces
// its shim, so one can be many releases behind. An install spanning six builds reported no per-session
// build at all, and identifying them from process inspection is what this replaces.
func TestShimBuildPrintsUnknownWhenAbsent(t *testing.T) {
	for _, tc := range []struct {
		name  string
		build string
		want  string
	}{
		{name: "reported", build: "abc123def456", want: "abc123def456"},
		{name: "not reported", build: "", want: "unknown"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			sess := &serverv1.Session{Id: "aaaaaaaa", Name: "s", ShimBuild: tc.build}
			if err := printSessionInfo(&buf, sess, "shim_build"); err != nil {
				t.Fatalf("printSessionInfo() error = %v", err)
			}
			if got := strings.TrimSpace(buf.String()); got != tc.want {
				t.Errorf("shim_build = %q, want %q", got, tc.want)
			}
		})
	}
}
