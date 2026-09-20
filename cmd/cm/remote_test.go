package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/remote"
	"github.com/chancez/cm/internal/sessionenv"
)

// runnableCommands returns every command a user can invoke, keyed as the remote lists key them.
func runnableCommands(t *testing.T) map[string]*cobra.Command {
	t.Helper()

	found := map[string]*cobra.Command{}
	var walk func(c *cobra.Command)
	walk = func(c *cobra.Command) {
		if c.RunE != nil || c.Run != nil {
			found[commandKey(c)] = c
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(newRootCommand())
	return found
}

// Every command is classified, so the next one added cannot silently answer about the wrong machine.
//
// This is the guard that matters, and it is modelled on TestNoSelfExportedVariableBindsToAFlag for the same
// reason: the lists are easy to keep right and easy to forget. A command that reads local files and is in
// neither list would, under --remote, report about the local machine while the user asked about another,
// which looks like it worked. Refusing is loud and harmless by comparison, so an unclassified command fails
// here rather than shipping.
func TestEveryCommandIsClassifiedForRemote(t *testing.T) {
	for name := range runnableCommands(t) {
		if machineLocal[name] {
			continue
		}
		if _, pending := remotePending[name]; pending {
			continue
		}
		// Reaching here is the claim that the command works against a server on another machine, which
		// holds for anything that only talks RPC. If it is not one of those, add it to machineLocal with
		// the reason, or to remotePending until it is wired.
		if !remoteCapable[name] {
			t.Errorf("`cm %s` is in none of machineLocal, remotePending, or remoteCapable; "+
				"classify it, or --remote will answer about the wrong machine", name)
		}
	}
}

// And nothing is classified twice, since the two refusals say different things and a command in both would
// get whichever the code happened to check first.
func TestNoCommandIsClassifiedTwice(t *testing.T) {
	for name := range machineLocal {
		if _, pending := remotePending[name]; pending {
			t.Errorf("`cm %s` is both machine-local and remote-pending", name)
		}
		if remoteCapable[name] {
			t.Errorf("`cm %s` is both machine-local and remote-capable", name)
		}
	}
	for name := range remotePending {
		if remoteCapable[name] {
			t.Errorf("`cm %s` is both remote-pending and remote-capable", name)
		}
	}
}

// Every classified command actually exists, so a renamed or deleted command does not leave an entry that
// silently stops applying.
func TestClassificationsNameRealCommands(t *testing.T) {
	commands := runnableCommands(t)
	for _, list := range []map[string]bool{machineLocal, remoteCapable} {
		for name := range list {
			if _, ok := commands[name]; !ok {
				t.Errorf("`cm %s` is classified for --remote but is not a command", name)
			}
		}
	}
	for name := range remotePending {
		if _, ok := commands[name]; !ok {
			t.Errorf("`cm %s` is classified for --remote but is not a command", name)
		}
	}
}

// A machine-local command refuses and says where to get the answer, because a refusal that does not is only
// half of one.
func TestMachineLocalCommandRefusesARemote(t *testing.T) {
	commands := runnableCommands(t)
	cmd, ok := commands["logs shim"]
	if !ok {
		t.Fatal("`cm logs shim` is missing, so this test is asserting nothing")
	}

	g := &globals{remote: "ssh://work:2222/opt/cm"}
	err := g.checkRemote(cmd, []string{"build"})
	if err == nil {
		t.Fatal("checkRemote() = nil, want a refusal")
	}
	// The whole suggestion, since each part of it is a way the message can be wrong: the port, the remote's
	// own cm, and both words of the subcommand.
	want := "ssh -p 2222 work /opt/cm logs shim build"
	if !strings.Contains(err.Error(), want) {
		t.Errorf("error %q does not suggest %q", err, want)
	}
}

// The not-wired-yet refusal still works, tested by putting something in the list rather than by relying on
// an entry being there: remotePending is empty now that attach and tui are wired, and a test over an empty
// map would pass while proving nothing. The mechanism has to keep working for the next entry.
func TestPendingCommandSaysItIsNotWiredYet(t *testing.T) {
	commands := runnableCommands(t)
	cmd, ok := commands["list"]
	if !ok {
		t.Fatal("`cm list` is missing, so this test is asserting nothing")
	}

	remotePending["list"] = []string{"-t"}
	t.Cleanup(func() { delete(remotePending, "list") })

	g := &globals{remote: "ssh://work"}
	err := g.checkRemote(cmd, []string{"build"})
	if err == nil {
		t.Fatal("checkRemote() = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "yet") {
		t.Errorf("error %q does not say this is not wired yet", err)
	}
	// -t, because a pending command is one that would be interactive, and ssh allocates no pty for a
	// command. Without it the suggestion fails in a way that looks like cm's fault.
	if !strings.Contains(err.Error(), "ssh -t work cm list build") {
		t.Errorf("error %q does not suggest an ssh with a terminal", err)
	}
}

// A remote-capable command is allowed through, which is the other half of the guard: a check that only ever
// refused would pass every test above while making --remote useless.
func TestRemoteCapableCommandIsAllowed(t *testing.T) {
	commands := runnableCommands(t)
	for _, name := range []string{"list", "kill", "send", "wait", "tag", "server stop", "attach", "tui"} {
		cmd, ok := commands[name]
		if !ok {
			t.Fatalf("`cm %s` is missing, so this test is asserting nothing", name)
		}
		g := &globals{remote: "ssh://work"}
		if err := g.checkRemote(cmd, nil); err != nil {
			t.Errorf("checkRemote(%s) = %v, want nil", name, err)
		}
	}
}

// A malformed remote is reported as that, whichever command was typed, rather than as a refusal about the
// command or as a connection failure later.
func TestCheckRemoteReportsAMalformedRemote(t *testing.T) {
	commands := runnableCommands(t)
	for _, name := range []string{"list", "doctor", "attach"} {
		g := &globals{remote: "http://work"}
		err := g.checkRemote(commands[name], nil)
		if err == nil {
			t.Fatalf("checkRemote(%s) = nil, want an error", name)
		}
		if !strings.Contains(err.Error(), "ssh://") {
			t.Errorf("error for %s is %q, which does not say what a remote looks like", name, err)
		}
	}
}

// Without a remote, nothing is refused. The local behavior of every command is unchanged by this flag
// existing, which is the property the whole change rests on.
func TestNothingIsRefusedWithoutARemote(t *testing.T) {
	g := &globals{}
	for name, cmd := range runnableCommands(t) {
		if err := g.checkRemote(cmd, nil); err != nil {
			t.Errorf("checkRemote(%s) with no remote = %v, want nil", name, err)
		}
	}
}

// The variable --remote reads must never be inherited by a session, or every cm command inside a remote
// session hops out again: to a third host, or into a session inside a session when the remote is this
// machine. bindEnv deriving it from the flag is wanted; a shell keeping it is not.
func TestRemoteIsNotInheritedByASession(t *testing.T) {
	if remoteEnvVar != "CM_REMOTE" {
		t.Fatalf("remoteEnvVar = %q, so the NoInherit entry below no longer matches", remoteEnvVar)
	}
	got := sessionenv.Inherit([]string{remoteEnvVar + "=ssh://work", "TERM=xterm-kitty"})
	want := []string{"TERM=xterm-kitty"}
	if len(got) != len(want) || got[0] != want[0] {
		t.Errorf("Inherit() = %q, want %q", got, want)
	}
}

// And it stays bindable from the environment, which is the point of having it as a variable: a window can be
// pointed at a host once. This is the half that TestNoSelfExportedVariableBindsToAFlag would forbid for a
// variable cm exports into a session, and CM_REMOTE is the first that is deliberately both.
func TestRemoteBindsFromTheEnvironment(t *testing.T) {
	if noEnvFlags["remote"] {
		t.Error("--remote is in noEnvFlags, so CM_REMOTE no longer works; " +
			"it is kept out on purpose, and sessionenv.NoInherit is what stops a session keeping it")
	}
}

// A remote attachment does not send this machine's environment to a shell on another one.
//
// What crosses is what describes the terminal; a macOS PATH in front of a Linux shell, or a HOME naming a
// directory that is not there, is the failure this avoids. Explicit --env still arrives, and last, so it
// wins.
func TestApplyRemoteSendsSshdsEnvironmentNotThisOne(t *testing.T) {
	// A whole environment, given rather than taken from this process, so the assertion below is about the
	// policy and not about whatever the developer has exported.
	environ := []string{
		"TERM=xterm-kitty",
		"PATH=/opt/homebrew/bin:/usr/bin",
		"HOME=/Users/someone",
		"KITTY_LISTEN_ON=unix:/tmp/kitty-1",
		"SSH_AUTH_SOCK=/tmp/agent.1",
		"LC_ALL=en_US.UTF-8",
		"AWS_SECRET_ACCESS_KEY=hunter2",
	}

	opts := client.Options{Env: []string{"PATH=/local", "HOME=/local"}}
	applyRemote(&opts, &remoteDialer{target: remote.Target{Host: "work"}}, environ, []string{"FOO=bar"})

	want := []string{"TERM=xterm-kitty", "LC_ALL=en_US.UTF-8", "FOO=bar"}
	if !slices.Equal(opts.Env, want) {
		t.Errorf("Env = %q, want %q", opts.Env, want)
	}
}

// And the local-only parts of an attachment are taken off rather than left pointing at this machine.
func TestApplyRemoteReplacesWhatIsLocal(t *testing.T) {
	opts := client.Options{
		SocketPath:    "/tmp/cm-501/server.sock",
		StartServer:   func(context.Context) error { return nil },
		ServerStopped: func() bool { return true },
		// Set from CM_SESSION by a client running inside a local session. It names a session on the local
		// server, so a remote server would resolve it to nothing or to something unrelated.
		InsideSession: "work",
	}
	applyRemote(&opts, &remoteDialer{target: remote.Target{Host: "work"}}, nil, nil)

	if opts.Dial == nil {
		t.Error("Dial is nil, so the attachment would still dial a socket on this machine")
	}
	if opts.SocketPath != "" {
		t.Errorf("SocketPath = %q, want it cleared", opts.SocketPath)
	}
	if opts.InsideSession != "" {
		t.Errorf("InsideSession = %q, want it cleared for a remote server", opts.InsideSession)
	}
	if opts.ServerStopped != nil {
		t.Error("ServerStopped is set, so a local `cm server stop` would suppress recovery of a remote server")
	}
	if opts.StartServer == nil {
		t.Error("StartServer is nil, so a client whose remote server died could not ask for it back")
	}
}

// The first dial asks the far end to start a server and later ones do not, which is what "creating one if
// needed" means without letting a reconnect defeat `cm server stop` on the remote.
func TestRemoteDialerStartsOnlyOnTheFirstDial(t *testing.T) {
	d := &remoteDialer{target: remote.Target{Host: "work"}}

	_, first := d.target.ProxyCommand(remote.Dialing{Start: !d.haveDialed})
	d.haveDialed = true
	_, second := d.target.ProxyCommand(remote.Dialing{Start: !d.haveDialed})

	if !slices.Contains(first, "--start") {
		t.Errorf("the first dial runs %q, which does not ask for a server", first)
	}
	if slices.Contains(second, "--start") {
		t.Errorf("a later dial runs %q, which would start a server a stop had just stopped", second)
	}
	// And the recovery path asks explicitly, which is the client's decision rather than the dialer's.
	_, recovery := d.target.ProxyCommand(remote.Dialing{Start: true})
	if !slices.Contains(recovery, "--start") {
		t.Errorf("the recovery command %q does not ask for a server", recovery)
	}
}

// A follower is pointed at the same server, which is the last place a --remote invocation could have
// streamed a local session of the same name.
func TestPointAtServerFollowsTheRemote(t *testing.T) {
	var remoteOpts client.Options
	g := &globals{remote: "ssh://work"}
	if err := g.pointAtServer(&remoteOpts); err != nil {
		t.Fatalf("pointAtServer() error = %v, want nil", err)
	}
	if remoteOpts.Dial == nil || remoteOpts.SocketPath != "" {
		t.Errorf("a remote follower got Dial=%v SocketPath=%q, want a dialer and no path",
			remoteOpts.Dial != nil, remoteOpts.SocketPath)
	}

	var localOpts client.Options
	local := &globals{}
	if err := local.pointAtServer(&localOpts); err != nil {
		t.Fatalf("pointAtServer() error = %v, want nil", err)
	}
	if localOpts.Dial != nil || localOpts.SocketPath == "" {
		t.Errorf("a local follower got Dial=%v SocketPath=%q, want a path and no dialer",
			localOpts.Dial != nil, localOpts.SocketPath)
	}
}

// Completion asks this machine when no remote is named, which is the case that must not regress: the flag
// existing changes nothing about a local shell.
func TestCompletionServerIsLocalWithoutARemote(t *testing.T) {
	g := &globals{configPath: "/nonexistent.toml"}

	ctx, cancel := g.completionDeadline(t.Context())
	defer cancel()
	// No server runs under this test's directories, so a failed dial is expected and is not what is asserted:
	// where it dialed is. An error mentioning ssh would mean a local completion went out over the network.
	_, _, err := g.completionServer(ctx)
	if err != nil && strings.Contains(err.Error(), "ssh") {
		t.Errorf("completionServer() error = %v, which mentions ssh for a local completion", err)
	}
}

// A remote is asked whether or not a connection is already shared, which is slower than a local completion
// and is the point: the names offered are read as the things the command will act on, so this machine's names
// under --remote would be a mistake the user cannot see they are making.
func TestCompletionServerAsksAColdRemote(t *testing.T) {
	// Sharing off, so there is certainly no connection to reuse and this is the cold path.
	path := filepath.Join(t.TempDir(), "cm.toml")
	body := "[remote]\nconnection_persist = \"0\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("WriteFile() error = %v, want nil", err)
	}
	g := &globals{remote: "ssh://cm-test-nonexistent.invalid", configPath: path}

	ctx, cancel := g.completionDeadline(t.Context())
	defer cancel()
	_, _, err := g.completionServer(ctx)
	if err == nil {
		t.Fatal("completionServer() error = nil against a host that does not resolve")
	}
	// It reached for the remote rather than falling back to this machine, which the message shows.
	if !strings.Contains(err.Error(), "ssh") {
		t.Errorf("completionServer() error = %v, which does not look like it asked the remote", err)
	}
}

// And it cannot hang on a keystroke. A host that is unreachable rather than slow gives up inside the bound,
// which is why there is a deadline rather than a refusal to ask at all.
func TestCompletionIsBounded(t *testing.T) {
	g := &globals{remote: "ssh://cm-test-nonexistent.invalid", configPath: "/nonexistent.toml"}

	began := time.Now()
	ctx, cancel := g.completionDeadline(t.Context())
	defer cancel()
	if _, _, err := g.completionServer(ctx); err == nil {
		t.Fatal("completionServer() error = nil against a host that does not resolve")
	}
	// Generous over the bound, since an ssh has to be started and reaped, and still far short of what a
	// missing deadline would produce: the handshake alone waits 30s.
	if waited := time.Since(began); waited > completionTimeout+5*time.Second {
		t.Errorf("completionServer() took %v, past the %v bound", waited, completionTimeout)
	}
}

// A malformed remote is an error rather than this machine's names, for the same reason a cold one is asked:
// the names come from where the user pointed, or from nowhere.
func TestCompletionServerRefusesAMalformedRemote(t *testing.T) {
	g := &globals{remote: "http://work", configPath: "/nonexistent.toml"}

	ctx, cancel := g.completionDeadline(t.Context())
	defer cancel()
	_, _, err := g.completionServer(ctx)
	if err == nil {
		t.Fatal("completionServer() error = nil for a malformed remote")
	}
	if !strings.Contains(err.Error(), "ssh://") {
		t.Errorf("error %q does not say what a remote looks like", err)
	}
}

// A completion never starts a server on another machine. Completing a name is a question, and starting a
// server is not part of asking it.
func TestCompletionNeverStartsARemoteServer(t *testing.T) {
	g := &globals{remote: "ssh://work", configPath: "/nonexistent.toml"}

	d, err := g.remoteDialerFor(true)
	if err != nil {
		t.Fatalf("remoteDialerFor() error = %v, want nil", err)
	}
	if !d.noStart {
		t.Fatal("the completion dialer would start a server")
	}
	_, args := d.target.ProxyCommand(remote.Dialing{Start: !d.noStart && !d.haveDialed})
	if slices.Contains(args, "--start") {
		t.Errorf("the completion command %q asks the remote to start a server", args)
	}
}
