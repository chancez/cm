package main

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/remote"
)

// remoteTarget reports the remote server this invocation names, or nil for this machine's.
func (g *globals) remoteTarget() (*remote.Target, error) {
	if g.remote == "" {
		return nil, nil
	}
	t, err := remote.Parse(g.remote)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

// machineLocal lists the commands that are about the machine they run on.
//
// Each reads local files, manages local processes, or replaces the local binary, so none can be answered
// over RPC at all. Every one of them has an answer on the remote and it is not one a local process can
// give, so these refuse and say how to get it. Answering about the wrong machine is the failure worth
// preventing, because it looks like it worked.
var machineLocal = map[string]bool{
	// Diagnoses this installation: socket paths, directory permissions, leaked shims, this build.
	"doctor": true,
	// Reports the configuration *this process* resolved, from this machine's files and environment.
	"config": true,
	// Reads log files out of the local state directory.
	"logs":        true,
	"logs client": true,
	"logs server": true,
	"logs shim":   true,
	// A server's own lifetime, and a process on this machine either way. `server stop` is the exception,
	// below, because stopping one is an RPC and works wherever the server is.
	"server":         true,
	"server restart": true,
	// The far end of somebody else's remote connection. Pointing it at a third machine would be a chain
	// nothing asks for and nothing tests.
	"server proxy": true,
	// Holds a pty for a session on this machine, spawned by a server, never typed.
	"shim": true,
	// Moves this machine's running server and clients onto the build installed here.
	"upgrade": true,
	// Print static text and talk to nothing.
	"completions": true,
	"shell-init":  true,
	// The root command, which prints help. Spelled as the empty string because a key here is a path with the
	// program name removed, and for the root there is nothing left.
	"": true,
}

// remoteCapable lists the commands that work against a server on another machine.
//
// Not consulted at runtime: a command in none of these three lists is allowed through, which is the same
// permissive default bindEnv has. This exists so TestEveryCommandIsClassifiedForRemote can fail on a command
// nobody has thought about, which is the only way the permissive default is safe.
//
// What they have in common is that everything they do is an RPC, so where the server is decides where the
// work happens and the client contributes nothing but the question.
var remoteCapable = map[string]bool{
	// Sessions: what exists, what it is doing, and ending it.
	"list":    true,
	"info":    true,
	"kill":    true,
	"run":     true,
	"history": true,
	"wait":    true,
	"report":  true,
	"get-env": true,
	// Input and output, minus the streaming forms: see remotePending and refusePendingFlag.
	"send": true,
	"read": true,
	// Names, tags, and what a client is pointed at.
	"bind":            true,
	"unbind":          true,
	"rebind":          true,
	"switch":          true,
	"tag":             true,
	"signal":          true,
	"detach":          true,
	"clients current": true,
	"clients list":    true,
	"clients upgrade": true,
	// Reports that describe a server, which is the one on the far end when there is one. Both dial without
	// starting anything, so neither brings a remote server into being in order to describe it.
	"status":  true,
	"version": true,
	// Stopping a server is an RPC, unlike starting or replacing one, so it works wherever the server is.
	"server stop": true,
}

// remotePending lists the commands that will work against a remote but do not yet.
//
// Separate from machineLocal because the reason is different and so is what to do about it: these are
// waiting on the client side of the work rather than being local by nature, and the entry disappears when
// each is wired. They refuse rather than quietly acting on the local server, which is the bug this list
// exists to prevent: `cm attach --remote host work` that attached to a *local* session named work would
// look like it worked.
//
// The value is the ssh options a suggested command needs. attach wants a terminal on the far end, and ssh
// allocates none for a command.
var remotePending = map[string][]string{
	"attach": {"-t"},
	// Hands the session it picks to a `cm attach` child, so it is pending for the same reason.
	"tui": {"-t"},
}

// checkRemote reports why this command cannot act on the remote it was given.
//
// One place rather than a guard per command, and a list rather than a judgement at each call site, for the
// reason noEnvFlags is a list: the next command added has an obvious home, and
// TestEveryCommandIsClassifiedForRemote fails until it is in one of them. Nil when nothing is wrong.
func (g *globals) checkRemote(cmd *cobra.Command, args []string) error {
	if g.remote == "" {
		return nil
	}
	// Parsed before either refusal, so a malformed remote is reported as that rather than as whichever
	// command happened to be typed.
	t, err := g.remoteTarget()
	if err != nil {
		return err
	}

	// The arguments are carried into the suggestion so it can be pasted rather than adapted: a refusal from
	// `cm logs shim build` that suggested `ssh host cm logs shim` leaves the user to notice what is missing.
	words := append(commandWords(cmd), args...)

	name := commandKey(cmd)
	if sshFlags, pending := remotePending[name]; pending {
		return fmt.Errorf(
			"%s cannot use --remote yet; run it through ssh yourself for now:\n    %s",
			cmd.CommandPath(), t.SuggestionWith(sshFlags, words...))
	}
	if machineLocal[name] {
		return fmt.Errorf(
			"%s is about the machine it runs on, so it cannot be pointed at a remote; run it there instead:\n"+
				"    %s",
			cmd.CommandPath(), t.Suggestion(words...))
	}
	return nil
}

// refusePendingFlag reports that one flag's path is not wired for a remote yet.
//
// For a command that is remote-capable except down one branch: `cm read` and `cm send` answer over RPC, and
// their --follow streams through internal/client, which has no remote dialer yet. Refusing the combination
// is the difference between a message and following the *local* server's session of the same name. Nil when
// there is no remote, so a call site is one guard beside the other flag checks.
func (g *globals) refusePendingFlag(cmd *cobra.Command, args []string, flag string) error {
	if g.remote == "" {
		return nil
	}
	t, err := g.remoteTarget()
	if err != nil {
		return err
	}
	return fmt.Errorf(
		"%s --%s cannot use --remote yet, because it streams the way an attachment does;\n"+
			"run it through ssh yourself for now:\n    %s",
		cmd.CommandPath(), flag,
		t.Suggestion(append(append(commandWords(cmd), args...), "--"+flag)...))
}

// commandKey names a command the way the lists above spell it: its path without the program name.
func commandKey(cmd *cobra.Command) string {
	return strings.Join(commandWords(cmd), " ")
}

// commandWords returns a command's path without the program name, as separate words.
//
// So `cm logs shim` suggests `ssh host cm logs shim` rather than `ssh host cm shim`, which is a different
// command that exists and does something else entirely.
func commandWords(cmd *cobra.Command) []string {
	words := strings.Fields(cmd.CommandPath())
	if len(words) > 0 {
		return words[1:]
	}
	return nil
}

// remoteEnvVar is the variable --remote reads, and the one a session must not inherit.
//
// bindEnv derives it from the flag, which is wanted here: a terminal window or a shell can be pointed at a
// host once and every cm command in it follows. What must not happen is a *session* inheriting it. `cm
// attach` forwards its environment into the session it creates, so a shell born on the remote would carry
// this, and every cm command run inside that session would hop out again -- to a third host, or, for a
// remote that is this machine, into a session inside a session inside a session.
//
// So it is listed in sessionenv.NoInherit and deliberately *not* in noEnvFlags: those two lists disagree
// here, and this is the first variable where that is correct. See the CM_SESSION and CM_RESUME_FROM_SEQ
// entries on noEnvFlags for what the other shape of this mistake cost.
var remoteEnvVar = paths.Env("REMOTE")
