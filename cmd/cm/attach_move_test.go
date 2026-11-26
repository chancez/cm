package main

import (
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// --rebind and --switch are refused before anything is created wherever they cannot move a window: together,
// with --no-attach or --read-only, outside a session, and against a remote given by address, which a window's
// name cannot point at.
func TestCheckMoveFlags(t *testing.T) {
	type args struct {
		rebind, switchWindow, noAttach, readOnly bool
		session, remote                          string
	}
	tests := []struct {
		name string
		args args
		want string
	}{
		{name: "neither", args: args{}, want: ""},
		{name: "rebind in a session", args: args{rebind: true, session: "@a1", remote: "white"}, want: ""},
		{name: "switch here", args: args{switchWindow: true, session: "@a1"}, want: ""},
		{name: "both", args: args{rebind: true, switchWindow: true, session: "@a1"}, want: "--rebind and --switch are two ways to move the window; give one"},
		{name: "no attach", args: args{rebind: true, noAttach: true, session: "@a1"}, want: "--rebind moves this window, which --no-attach leaves where it is"},
		{name: "read only", args: args{switchWindow: true, readOnly: true, session: "@a1"}, want: "--switch moves this window to the session as it is, not as a follower"},
		{name: "outside a session", args: args{rebind: true, remote: "white"}, want: "--rebind moves the window this runs in, and this is not in a cm session"},
		{name: "address", args: args{rebind: true, session: "@a1", remote: "ssh://me@white"}, want: "--rebind needs the host by its name in [remote.hosts], not ssh://me@white"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			a := tt.args
			got := ""
			if err := checkMoveFlags(a.rebind, a.switchWindow, a.noAttach, a.readOnly, a.session, a.remote); err != nil {
				got = err.Error()
			}
			if got != tt.want {
				t.Errorf("checkMoveFlags() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The window moves to what was typed, so a name stays a name, else to the ID the server allocated, qualified
// by the host the session was made on.
func TestMoveTarget(t *testing.T) {
	opened := &serverv1.Opened{Session: "@x9", SessionId: "x9"}
	got := []string{
		moveTarget("white", "work", opened),
		moveTarget("white", "", opened),
		moveTarget("", "work", opened),
		moveTarget("", "", opened),
	}
	want := []string{"white:work", "white:@x9", "work", "@x9"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("moveTarget = %q, want %q", got, want)
	}
}
