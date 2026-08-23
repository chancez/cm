package main

import (
	"bytes"
	"strings"
	"testing"
)

// The report says one thing on a healthy run, and the cases differ only in the verb.
//
// Asserted on the whole line rather than on fragments, since what is being pinned is the shape: an upgrade
// that printed three lines about things nobody can act on is what this replaced.
func TestPrintUpgradeReport(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  upgradeReport
		want string
	}{
		{
			name: "a new build, with a client on it",
			out: upgradeReport{
				ServerBefore: "v0.2.1", ServerAfter: "v0.3.0",
				Asked:           map[string]uint32{"work": 1},
				KeptShims:       2,
				ClientsBefore:   1,
				ClientsReturned: 1,
			},
			want: "upgraded to v0.3.0, 1 client\n",
		},
		{
			// The same build is a restart rather than an upgrade: reporting "v0.3.0 -> v0.3.0" as a change
			// would be a lie, and running this without installing anything is reasonable.
			name: "the same build",
			out: upgradeReport{
				ServerBefore: "v0.3.0", ServerAfter: "v0.3.0",
				AlreadyOn:       map[string]uint32{"work": 2},
				ClientsBefore:   2,
				ClientsReturned: 2,
			},
			want: "restarted on v0.3.0, 2 clients\n",
		},
		{
			name: "nothing was running",
			out:  upgradeReport{ServerAfter: "v0.3.0"},
			want: "started on v0.3.0\n",
		},
		{
			// A kept shim is true on every run where a session exists, so it is not said at all. It stays
			// in --json for anything that wants the number.
			name: "sessions but no clients says nothing about either",
			out: upgradeReport{
				ServerBefore: "v0.2.1", ServerAfter: "v0.3.0",
				Asked:     map[string]uint32{"work": 0},
				KeptShims: 4,
			},
			want: "upgraded to v0.3.0\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			if err := printUpgradeReport(&buf, tc.out); err != nil {
				t.Fatalf("printUpgradeReport() error = %v", err)
			}
			if got := buf.String(); got != tc.want {
				t.Errorf("printUpgradeReport() = %q, want %q", got, tc.want)
			}
		})
	}
}

// A client that never came back is a window left on the old build, which is the one outcome here worth
// interrupting for. It goes to stderr rather than stdout, so a script reading the build is unaffected, and
// stdout stays one line.
func TestPrintUpgradeReportKeepsStdoutCleanWhenAClientIsMissing(t *testing.T) {
	var buf bytes.Buffer
	err := printUpgradeReport(&buf, upgradeReport{
		ServerBefore: "v0.2.1", ServerAfter: "v0.3.0",
		Asked:           map[string]uint32{"work": 1},
		ClientsBefore:   3,
		ClientsReturned: 1,
	})
	if err != nil {
		t.Fatalf("printUpgradeReport() error = %v", err)
	}
	if got, want := buf.String(), "upgraded to v0.3.0, 1 client\n"; got != want {
		t.Errorf("stdout = %q, want %q: the warning belongs on stderr", got, want)
	}
	if strings.Contains(buf.String(), "reconnect") {
		t.Error("the shortfall was written to stdout, want it on stderr")
	}
}
