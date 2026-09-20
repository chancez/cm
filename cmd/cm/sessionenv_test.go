package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/sessionenv"
)

// A small environment rather than the process's own, which matters more than it looks. These tests
// assert on the whole returned slice, so a failure prints it, and an earlier version of this file
// called through to os.Environ() and dumped the developer's real environment including API tokens
// into the test output. Test failures end up in terminals, CI logs, and pasted bug reports.
var fakeClientEnv = []string{
	"PATH=/client/bin",
	"HOME=/home/someone",
	"FOO=bar",
}

// --env has to beat the forwarded value, which is a property of the order sessionEnv builds rather
// than of anything the server does. Asserted on the whole slice: a test that only checked the
// explicit value was present would pass with the order reversed, where the flag is silently
// shadowed by the forwarded entry and appears to do nothing.
func TestSessionEnvPutsExplicitEntriesLast(t *testing.T) {
	got := sessionEnvFrom(fakeClientEnv, []string{"PATH=/explicit", "NEW=1"})
	want := []string{
		"PATH=/client/bin", "HOME=/home/someone", "FOO=bar",
		"PATH=/explicit", "NEW=1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionEnvFrom() = %+v, want %+v", got, want)
	}
}

// With no --env, a session gets the client's environment as it stands. This is the path every
// ordinary `cm attach` takes.
func TestSessionEnvForwardsTheWholeClientEnvironment(t *testing.T) {
	got := sessionEnvFrom(fakeClientEnv, nil)
	want := []string{"PATH=/client/bin", "HOME=/home/someone", "FOO=bar"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionEnvFrom() = %+v, want %+v", got, want)
	}
}

// The dynamic linker variables are dropped, since they choose what code a process loads rather than
// how it behaves. sshd defaults PermitUserEnvironment to no for this reason.
func TestSessionEnvDropsLinkerVariables(t *testing.T) {
	got := sessionEnvFrom([]string{
		"PATH=/client/bin",
		"LD_PRELOAD=/tmp/evil.so",
		"DYLD_INSERT_LIBRARIES=/tmp/evil.dylib",
		"FOO=bar",
	}, nil)
	want := []string{"PATH=/client/bin", "FOO=bar"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionEnvFrom() = %+v, want %+v", got, want)
	}
}

// Nothing here filters by name beyond the linker variables, so a caller's own secrets do travel to
// the session it creates. That is the deliberate consequence of forwarding, and it is asserted so
// the decision is visible rather than implied: a session gets what the client had, exactly as a
// terminal split does.
func TestSessionEnvForwardsEverythingElseIncludingSecrets(t *testing.T) {
	got := sessionEnvFrom([]string{"MY_API_TOKEN=shhh"}, nil)
	if !reflect.DeepEqual(got, []string{"MY_API_TOKEN=shhh"}) {
		t.Errorf("sessionEnvFrom() = %+v, want the value forwarded unchanged", got)
	}
}

// A client with almost no environment forwards almost nothing, rather than being topped up here. The
// server's environment is the floor for anything missing, and that decision lives in shimEnv.
func TestSessionEnvWithASparseClient(t *testing.T) {
	got := sessionEnvFrom([]string{"PATH=/usr/bin:/bin"}, nil)
	want := []string{"PATH=/usr/bin:/bin"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionEnvFrom() = %+v, want %+v", got, want)
	}
}

// Guards the reason sessionEnvFrom exists: if the seam is removed and this reads the real
// environment again, a failure elsewhere in this file leaks it.
func TestSessionEnvDoesNotReadTheProcessEnvironment(t *testing.T) {
	t.Setenv("CM_TEST_LEAK_CANARY", "must-not-appear")

	got := sessionEnvFrom(fakeClientEnv, nil)
	for _, kv := range got {
		if strings.HasPrefix(kv, "CM_TEST_LEAK_CANARY=") {
			t.Fatalf("sessionEnvFrom() read the process environment: %q", kv)
		}
	}
}

// A session created on another machine gets sshd's posture, whichever command creates it.
//
// The regression this guards: `cm run --remote` built its own Open and called sessionEnv, so a session on the
// far host got this machine's entire environment while `cm attach --remote` got the short cross-host list.
// Observed rather than reasoned about, by reading the environment inside a session `cm run --remote` created:
// the local sandbox's CM_RUNTIME_DIR and CM_CONFIG were in it, along with an SSH_AUTH_SOCK naming a socket
// that exists only here, and any credential in the calling shell would have crossed with them.
func TestSessionEnvForARemoteSendsOnlyWhatCrossesAHost(t *testing.T) {
	environ := []string{
		"TERM=xterm-kitty",
		"LANG=en_US.UTF-8",
		"PATH=/opt/homebrew/bin",
		"HOME=/Users/someone",
		"CM_RUNTIME_DIR=/tmp/cm-sandbox/t1/r",
		"SSH_AUTH_SOCK=/Users/someone/.cache/ssh/agent.sock",
		"AWS_SECRET_ACCESS_KEY=hunter2",
	}

	g := &globals{remote: "ssh://work"}
	got := g.sessionEnvFor(environ, []string{"FOO=bar"})
	want := []string{
		"TERM=xterm-kitty",
		"LANG=en_US.UTF-8",
		sessionenv.ClientHostVar + "=" + clientHostname(),
		"FOO=bar",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionEnvFor() = %q, want %q", got, want)
	}
}

// And without a remote the environment is unchanged, which is the half that makes the flag's absence provable
// rather than assumed: every session created on this machine still resembles the client that created it.
func TestSessionEnvForThisMachineIsTheOrdinaryOne(t *testing.T) {
	g := &globals{}
	got := g.sessionEnvFor(fakeClientEnv, []string{"FOO=baz"})
	want := sessionEnvFrom(fakeClientEnv, []string{"FOO=baz"})
	if !reflect.DeepEqual(got, want) {
		t.Errorf("sessionEnvFor() = %q, want %q", got, want)
	}
}
