package main

import (
	"strings"
	"testing"

	"github.com/spf13/cobra"

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

// A command that is not wired yet refuses too, and says that rather than that it is local, because the two
// are different facts and only one of them will still be true next month.
func TestPendingCommandSaysItIsNotWiredYet(t *testing.T) {
	commands := runnableCommands(t)
	cmd, ok := commands["attach"]
	if !ok {
		t.Fatal("`cm attach` is missing, so this test is asserting nothing")
	}

	g := &globals{remote: "ssh://work"}
	err := g.checkRemote(cmd, []string{"build"})
	if err == nil {
		t.Fatal("checkRemote() = nil, want a refusal")
	}
	if !strings.Contains(err.Error(), "yet") {
		t.Errorf("error %q does not say this is not wired yet", err)
	}
	// -t, because the suggested command runs an interactive cm on the far end and ssh allocates no pty for
	// a command. Without it the suggestion fails in a way that looks like cm's fault.
	if !strings.Contains(err.Error(), "ssh -t work cm attach build") {
		t.Errorf("error %q does not suggest an ssh with a terminal", err)
	}
}

// A remote-capable command is allowed through, which is the other half of the guard: a check that only ever
// refused would pass every test above while making --remote useless.
func TestRemoteCapableCommandIsAllowed(t *testing.T) {
	commands := runnableCommands(t)
	for _, name := range []string{"list", "kill", "send", "wait", "tag", "server stop"} {
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
