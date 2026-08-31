package main

import (
	"strings"
	"testing"

	"github.com/chancez/cm/internal/paths"
)

// splitBindArgs resolves both arguments, and which one is which is the part worth pinning.
//
// A bind takes a name first and a session second, which is the reverse of every other command's single
// session argument. Reading the session from args[0] would bind a name to itself, and with CM_SESSION set
// that mistake reaches the server and succeeds.
func TestSplitBindArgs(t *testing.T) {
	tests := []struct {
		desc        string
		args        []string
		env         string
		wantName    string
		wantSession string
		wantErr     string
	}{
		{
			desc: "both given", args: []string{"refactor", "@a7k2m9x4"},
			wantName: "refactor", wantSession: "@a7k2m9x4",
		},
		{
			desc: "a session given by name", args: []string{"build", "work"},
			wantName: "build", wantSession: "work",
		},
		{
			// The whole point of the fallback: naming the window in front of you is one word.
			desc: "the calling session when none is given", args: []string{"refactor"},
			env: "@yych7tdc", wantName: "refactor", wantSession: "@yych7tdc",
		},
		{
			// An explicit session wins, so a script inside one session can still name another.
			desc: "an explicit session overrides the environment", args: []string{"refactor", "work"},
			env: "@yych7tdc", wantName: "refactor", wantSession: "work",
		},
		{
			desc: "no session and nothing to fall back on", args: []string{"refactor"},
			wantErr: "no session to bind a name to",
		},
		{
			// Reported as the ID rule rather than as a missing session, even with nothing to fall back
			// on: the argument that is wrong is the one that was supplied.
			desc: "an ID cannot be bound", args: []string{"@a7k2m9x4"},
			wantErr: "cannot be bound",
		},
		{
			desc: "a name that is not a legal name", args: []string{"has spaces", "work"},
			env: "@yych7tdc", wantErr: "disallowed character",
		},
	}
	for _, tt := range tests {
		t.Run(tt.desc, func(t *testing.T) {
			t.Setenv(paths.SessionEnv(), tt.env)

			name, session, err := splitBindArgs(tt.args)
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("splitBindArgs(%q) error = %v, want one containing %q",
						tt.args, err, tt.wantErr)
				}
				if name != "" || session != "" {
					t.Errorf("splitBindArgs(%q) = %q, %q on error, want both empty",
						tt.args, name, session)
				}
				return
			}
			if err != nil {
				t.Fatalf("splitBindArgs(%q) error = %v", tt.args, err)
			}
			if name != tt.wantName || session != tt.wantSession {
				t.Errorf("splitBindArgs(%q) = %q, %q, want %q, %q",
					tt.args, name, session, tt.wantName, tt.wantSession)
			}
		})
	}
}
