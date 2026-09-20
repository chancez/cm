package remote

import (
	"reflect"
	"strings"
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
	name, args := target.ProxyCommand(Dialing{})
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
	_, args := target.ProxyCommand(Dialing{Start: true})
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
}

// One shared connection per target, so every cm command after the first is cheap. Measured on loopback at
// 120 to 150ms for a fresh connection against 10 to 30ms through an existing one, which is the difference
// between a remote being usable and merely possible.
func TestProxyCommandSharesAConnection(t *testing.T) {
	target, err := Parse("ssh://work")
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	_, args := target.ProxyCommand(Dialing{ControlDir: "/tmp/cmtest"})

	joined := strings.Join(args, " ")
	for _, want := range []string{
		"ControlMaster=auto",
		"ControlPath=/tmp/cmtest/ssh-",
		"ControlPersist=60",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("args %q do not ask for %q", joined, want)
		}
	}
}

// Two targets that differ at all get their own connection, so a command meant for one host cannot ride a
// connection to another. The port is the case worth asserting: same host, different server.
func TestControlPathDistinguishesTargets(t *testing.T) {
	paths := map[string]string{}
	for _, ref := range []string{
		"ssh://work",
		"ssh://work:2222",
		"ssh://other@work",
		"ssh://elsewhere",
	} {
		target, err := Parse(ref)
		if err != nil {
			t.Fatalf("Parse(%q) error = %v, want nil", ref, err)
		}
		path := target.ControlPath("/tmp/cmtest")
		if path == "" {
			t.Fatalf("ControlPath() for %q is empty, so multiplexing is off for a path that fits", ref)
		}
		if other, clash := paths[path]; clash {
			t.Errorf("%q and %q share the control socket %q", ref, other, path)
		}
		paths[path] = ref
	}
}

// A directory that would overflow the socket limit turns multiplexing off rather than producing a path that
// fails at bind with an opaque EINVAL. The same hazard is why cm does not use ssh's own %C token.
func TestControlPathRefusesAPathThatWouldNotBind(t *testing.T) {
	target, err := Parse("ssh://work")
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	deep := "/" + strings.Repeat("d", 100)
	if got := target.ControlPath(deep); got != "" {
		t.Errorf("ControlPath(%d-byte dir) = %q, want empty", len(deep), got)
	}
	if got := target.ControlPath(""); got != "" {
		t.Errorf("ControlPath(\"\") = %q, want empty", got)
	}
}

// A configured command replaces ssh and keeps cm's options, since those are what make the byte stream
// survive: a wrapper is expected to be ssh-compatible, and one that is not fails at the banner rather than
// corrupting a session.
func TestProxyCommandHonorsAConfiguredCommand(t *testing.T) {
	target, err := Parse("ssh://work")
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}

	name, args := target.ProxyCommand(Dialing{Command: []string{"kitten", "ssh"}})
	if name != "kitten" {
		t.Errorf("program = %q, want %q", name, "kitten")
	}
	want := []string{
		"ssh",
		"-T",
		"-e", "none",
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
		"work",
		"--", "cm", "server", "proxy",
	}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("args =\n%q\nwant\n%q", args, want)
	}
}

// And the suggestion a refusal prints uses it too, so a copied command matches how cm itself connects.
func TestSuggestionHonorsAConfiguredCommand(t *testing.T) {
	target, err := Parse("ssh://work")
	if err != nil {
		t.Fatalf("Parse() error = %v, want nil", err)
	}
	got := target.Suggestion([]string{"kitten", "ssh"}, "doctor")
	want := "kitten ssh work cm doctor"
	if got != want {
		t.Errorf("Suggestion() = %q, want %q", got, want)
	}
}
