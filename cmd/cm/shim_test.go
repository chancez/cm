package main

import "testing"

// What a shim accepts as the session it serves, which is whatever the server that spawned it passes.
//
// The case that matters is a name with a dot. A shim is re-exec'd from the binary on disk, so a running
// older server spawns a new shim as soon as the binary is replaced, and it passes a name rather than an
// ID. Rejecting one made the shim exit before binding its socket, and the server then waited its full ten
// seconds for a socket that would never appear: `shim for kitty.2 did not become ready: timed out after
// 10s`, measured at 10.38s per attempt against a kitty split.
func TestValidateShimSession(t *testing.T) {
	tests := []struct {
		name    string
		session string
		wantErr bool
	}{
		{name: "an ID, what a current server passes", session: "a7k2m9x4"},
		{name: "a name, what an older server passes", session: "work"},
		{name: "a per-window name, whose dot is not legal in an ID", session: "kitty.325"},
		{name: "a name with the other legal punctuation", session: "my-session_2"},
		{name: "empty", session: "", wantErr: true},
		// The reason this is validated at all: the value names a socket and a log file.
		{name: "traversal", session: "../evil", wantErr: true},
		{name: "a separator", session: "a/b", wantErr: true},
		// The sigil belongs to a reference a user types, never to this value.
		{name: "a reference rather than a bare value", session: "@a7k2m9x4", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateShimSession(tt.session)
			if (err != nil) != tt.wantErr {
				t.Errorf("validateShimSession(%q) error = %v, wantErr %v", tt.session, err, tt.wantErr)
			}
		})
	}
}
