package remote

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    Target
		wantErr bool
	}{
		{name: "host", in: "ssh://work", want: Target{Host: "work"}},
		{name: "user and host", in: "ssh://chancez@work", want: Target{User: "chancez", Host: "work"}},
		{name: "port", in: "ssh://work:2222", want: Target{Host: "work", Port: 2222}},
		{
			name: "everything",
			in:   "ssh://chancez@work:2222/usr/local/bin/cm",
			want: Target{User: "chancez", Host: "work", Port: 2222, Command: "/usr/local/bin/cm"},
		},
		// The bare form ssh itself takes, because an ssh alias is what a user types everywhere else.
		{name: "bare host", in: "work", want: Target{Host: "work"}},
		{name: "bare user and host", in: "chancez@work", want: Target{User: "chancez", Host: "work"}},
		{name: "bare with port", in: "work:2222", want: Target{Host: "work", Port: 2222}},
		// A hostname is not resolved here, so an alias survives: the user's ssh config knows it and a
		// listing should show what they typed.
		{name: "an alias is kept", in: "ssh://prod-jump", want: Target{Host: "prod-jump"}},

		// A typo'd scheme is a refusal rather than a hostname nothing can resolve.
		{name: "wrong scheme", in: "sssh://work", wantErr: true},
		{name: "another scheme", in: "docker://work", wantErr: true},
		// A password would be in every shell history and in ps, and ssh would not use it.
		{name: "password", in: "ssh://chancez:hunter2@work", wantErr: true},
		{name: "no host", in: "ssh://", wantErr: true},
		{name: "empty", in: "", wantErr: true},
		{name: "only a user", in: "chancez@", wantErr: true},
		{name: "no user", in: "@work", wantErr: true},
		{name: "port is not a number", in: "ssh://work:http", wantErr: true},
		{name: "port out of range", in: "ssh://work:99999", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Parse(tt.in)
			if (err != nil) != tt.wantErr {
				t.Fatalf("Parse(%q) error = %v, wantErr %v", tt.in, err, tt.wantErr)
			}
			if tt.wantErr {
				return
			}
			if !reflect.DeepEqual(got, tt.want) {
				t.Errorf("Parse(%q) = %+v, want %+v", tt.in, got, tt.want)
			}
		})
	}
}

// A target renders back to something Parse accepts, which `cm switch` depends on: a switch re-execs the
// client as `cm attach @<id>`, and a remote one has to stay pointed at the same host.
func TestStringRoundTrips(t *testing.T) {
	for _, in := range []string{
		"ssh://work",
		"ssh://chancez@work",
		"ssh://work:2222",
		"ssh://chancez@work:2222/usr/local/bin/cm",
	} {
		t.Run(in, func(t *testing.T) {
			first, err := Parse(in)
			if err != nil {
				t.Fatalf("Parse(%q) error = %v, want nil", in, err)
			}
			if got := first.String(); got != in {
				t.Errorf("String() = %q, want %q", got, in)
			}
			again, err := Parse(first.String())
			if err != nil {
				t.Fatalf("Parse(%q) error = %v, want nil", first.String(), err)
			}
			if !reflect.DeepEqual(again, first) {
				t.Errorf("parsing the rendering gave %+v, want %+v", again, first)
			}
		})
	}
}

// The whole argv, because every option in it is there for a failure it prevents and dropping one would
// still connect on a good day: no pty so the line discipline cannot rewrite the protocol's newlines, no
// escape character so a "\n~." inside a message cannot kill the link, BatchMode so a passphrase prompt
// cannot land on a terminal a client is painting, and keepalives so a dropped link is an error rather than
// a hang.
func TestProxyCommand(t *testing.T) {
	want := []string{
		"-T",
		"-e", "none",
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"chancez@work",
		"--", "cm", "server", "proxy",
	}

	target, err := Parse("ssh://chancez@work")
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	name, args := target.ProxyCommand(false)
	if name != "ssh" {
		t.Errorf("program = %q, want %q", name, "ssh")
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
}

func TestProxyCommandWithEverything(t *testing.T) {
	want := []string{
		"-T",
		"-e", "none",
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"-p", "2222",
		"work",
		"--", "/usr/local/bin/cm", "server", "proxy", "--start",
	}

	target, err := Parse("ssh://work:2222/usr/local/bin/cm")
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	_, args := target.ProxyCommand(true)
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
}
