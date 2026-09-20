package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/remote"
	"github.com/chancez/cm/internal/sessionenv"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
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

// remoteDialer opens connections to a remote cm server for a client that reconnects.
//
// Stateful because the decision to start a server belongs to the first contact and to nothing after it.
// `cm attach --remote host work` on a host with no server running should bring one up, which is what
// "creating one if needed" means; every dial after that is a reconnect during an outage, and one that
// started a server would defeat `cm server stop` on the remote from any window that happened to retry.
// After the first, starting is the client's own decision, which it makes through StartServer below.
//
// No locking, because the two methods are called from the same loop in client.Attach: the dial at the top
// of each attempt, and the starter from the outage path inside it, never concurrently. A mutex here would
// suggest otherwise.
type remoteDialer struct {
	target remote.Target
	// command is the ssh command line, empty for plain ssh. See remote.Dialing.Command.
	command []string
	// persist is how long a shared connection is kept, zero for no sharing. From the config file.
	persist time.Duration
	// controlDir is where a shared ssh connection keeps its control socket, empty for a connection of this
	// command's own. See remote.Target.ControlPath.
	controlDir string
	// noStart suppresses asking the far end for a server, for the callers that must not bring one into being:
	// a report describing a server, and a request to stop one.
	noStart    bool
	haveDialed bool
}

// Dial opens a connection, starting a server on the far end only on the first attempt.
func (d *remoteDialer) Dial(ctx context.Context) (transport.Conn, serverv1.ServerClient, error) {
	start := !d.noStart && !d.haveDialed
	d.haveDialed = true

	name, args := d.target.ProxyCommand(remote.Dialing{
		Command:        d.command,
		ControlDir:     d.controlDir,
		ControlPersist: d.persist,
		Start:          start,
	})
	conn, cl, _, err := transport.DialServerVia(ctx, name, args...)
	return conn, cl, err
}

// StartServer brings a server up on the far end, for the client's recovery path.
//
// A connection opened and dropped, rather than a command of its own, because `cm server proxy --start` is
// already exactly this: the proxy starts a server and then has nothing more to do than be closed. It costs
// one extra ssh on a path that only runs after an outage has outlasted the quiet period.
func (d *remoteDialer) StartServer(ctx context.Context) error {
	name, args := d.target.ProxyCommand(remote.Dialing{
		Command:        d.command,
		ControlDir:     d.controlDir,
		ControlPersist: d.persist,
		Start:          true,
	})
	conn, _, _, err := transport.DialServerVia(ctx, name, args...)
	if err != nil {
		return err
	}
	return conn.Close()
}

// remoteDialerFor builds a dialer for the remote this invocation names, or nil when the server is local.
//
// The one place that decides where a shared ssh connection lives, which is this machine's runtime directory:
// the control socket is a local socket to a local ssh, so it belongs with cm's other sockets and is swept
// with them. Created here because a remote-only command otherwise never makes that directory, and ssh cannot
// bind a socket in a directory that is not there.
//
// A directory that cannot be made is not fatal. Multiplexing is an optimisation, and one fresh connection
// per command is how this worked before it existed, so the dialer just goes without.
func (g *globals) remoteDialerFor(noStart bool) (*remoteDialer, error) {
	target, err := g.remoteTarget()
	if err != nil {
		return nil, err
	}
	if target == nil {
		return nil, nil
	}

	command, persist, err := g.remoteSettings()
	if err != nil {
		return nil, err
	}

	d := &remoteDialer{target: *target, command: command, persist: persist, noStart: noStart}
	// Only when a connection is to be shared, so a zero persist leaves no socket behind and creates no
	// directory for one.
	if persist > 0 {
		if dirs, err := g.dirs(); err == nil {
			if err := dirs.Ensure(); err == nil {
				d.controlDir = dirs.Runtime
			}
		}
	}
	return d, nil
}

// remoteSettings resolves how to reach a remote: the ssh command line and how long to share a connection.
//
// Flag, then environment, then the config file, which is the precedence every other setting has. bindEnv has
// already folded the environment into the flag by the time this runs, so the only choice left here is
// between what was passed and what the file says.
func (g *globals) remoteSettings() ([]string, time.Duration, error) {
	cfg, err := g.config()
	if err != nil {
		return nil, 0, err
	}
	persist, err := cfg.RemoteConnectionPersist()
	if err != nil {
		return nil, 0, err
	}

	return g.sshCommandLine(), persist, nil
}

// serverFor connects to the server an attachment's Options names.
//
// Its dialer when it has one, this machine's socket otherwise, so the two paths that do part of an
// attachment's work outside client.Attach -- creating a session without attaching, and detaching from one --
// reach the same server the attachment would have. A local dial there is how `cm run --remote` would have
// created its session on the wrong machine.
func serverFor(
	ctx context.Context,
	dirs paths.Dirs,
	opts client.Options,
) (transport.Conn, serverv1.ServerClient, error) {
	if opts.Dial != nil {
		return opts.Dial(ctx)
	}
	return dialServer(dirs)
}

// ensureServer starts a server for this invocation if none is running, wherever that server belongs.
//
// The counterpart of connect for the callers that want a server to exist before doing something else, and
// the reason it is a method: `ensureServer(ctx, dirs)` starts one on *this* machine, which under --remote is
// a server nobody asked for while the request goes to another host.
func (g *globals) ensureServer(ctx context.Context) error {
	d, err := g.remoteDialerFor(false)
	if err != nil {
		return err
	}
	if d != nil {
		return d.StartServer(ctx)
	}

	dirs, err := g.dirs()
	if err != nil {
		return err
	}
	return ensureServer(ctx, dirs)
}

// pointAtServer tells an attachment which server to connect to, local or remote.
//
// For the followers, which build their own Options rather than going through attach's assembly: `cm read
// --follow` and `cm send --follow` are attachments in every respect that matters here, and they were the
// last place where a --remote invocation could have streamed a *local* session of the same name.
//
// Deliberately no StartServer, matching what a follower does locally: it never brings a server into being,
// because by the time it runs the command has already talked to one. The first dial still starts a remote
// server if there is somehow none, which is remoteDialer's own policy and costs nothing when one is there.
func (g *globals) pointAtServer(opts *client.Options) error {
	d, err := g.remoteDialerFor(false)
	if err != nil {
		return err
	}
	if d == nil {
		dirs, err := g.dirs()
		if err != nil {
			return err
		}
		opts.SocketPath = dirs.ServerSocket()
		return nil
	}

	opts.Dial = d.Dial
	return nil
}

// applyRemote points an attachment at a cm server on another machine.
//
// Everything it changes is something that was resolved locally and is wrong across a link, gathered here
// rather than spread through the option assembly so the whole policy can be read at once and so a local
// attach is provably untouched: nothing below runs unless there is a remote.
// environ is passed in rather than read here, following sessionEnvFrom: a test asserting on the whole
// resulting environment would otherwise depend on the developer's own, and print it on failure.
func applyRemote(opts *client.Options, dialer *remoteDialer, environ, env []string) {
	opts.Dial = dialer.Dial
	// Replaces the local recovery, which spawns a server process here. There is nothing on this machine to
	// recover: the server that matters is on the far end, and this asks it to come back.
	opts.StartServer = dialer.StartServer
	// Dropped rather than reimplemented. The marker it reads is a file in *this* machine's runtime
	// directory, so honoring it here would let a local `cm server stop` suppress recovery of a remote
	// server that was never stopped. The remote's own marker is honored where it lives, by the
	// `cm server proxy --start` that reads it.
	opts.ServerStopped = nil
	// The socket path is this machine's and now names nothing relevant. Cleared so a reader of a log line
	// or a panic is not shown a path that was never dialed.
	opts.SocketPath = ""

	// sshd's posture rather than this client's environment. See sessionenv.CrossHost: a shell on another
	// host builds its own PATH and HOME, and what it cannot know is the terminal drawing its output.
	// Explicit --env still wins, and comes last for that reason.
	opts.Env = append(sessionenv.CrossHost(environ), env...)

	// Not sent to a server that has never heard of it. InsideSession names a session on the *local* server,
	// which is where this client is running; the Open goes to the remote one, where the name either resolves
	// to nothing or, worse, matches an unrelated session and makes it stop attributing its own output to
	// itself.
	//
	// Clearing it is also what makes the local parent hear about this client, which reads backwards and is
	// the whole design: newNestingAnnouncer announces over this client's own output precisely when Open named
	// no parent, since that is the case where no server knows the nesting. So a --remote attach nested inside
	// a local session hands the detach key to the inner client the same as a local nesting does, over the pty
	// rather than through an RPC, and three presses escape a handover nothing is acting on.
	opts.InsideSession = ""
}

// completionTimeout bounds a completion that has to reach another machine.
//
// A bound rather than a refusal, which is the whole difference: correct names slowly are worth more than no
// names, and that is a judgement about what a completion is for rather than about latency. What a bound
// prevents is the other failure, where a host that is unreachable rather than slow leaves the prompt frozen
// on a keystroke with nothing to show for it.
//
// Five seconds covers a fresh ssh to a far host, which is a few hundred milliseconds on a normal link and
// more over a slow VPN, while being short enough that a dead host is an empty completion rather than a stuck
// terminal. Erring long is the right direction here: a completion that gives up on a working host is the
// thing being fixed.
//
// The deadline covers the whole completion, the connection and the request both, because it is the keystroke
// that is being bounded rather than any one step of it. exec.CommandContext means it reaches the ssh too, so
// a hung TCP connect is bounded without cm having to set ssh's own ConnectTimeout.
const completionTimeout = 5 * time.Second

// completionDeadline bounds a completion, whichever machine it ends up asking.
//
// Applied to a local completion as well, where it can never fire: a local dial is a unix socket and answers
// in about 20ms. One rule is easier to reason about than a rule with an exception, and a local completion
// that somehow took five seconds would be a bug worth surfacing rather than waiting for.
func (g *globals) completionDeadline(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(ctx, completionTimeout)
}

// completionServer connects to whichever server a completion should ask.
//
// The remote when one is named, opening a connection if none is shared yet, which is slower than a local
// completion and the point: the names a completion offers are read as the things the command will act on, so
// this machine's names under --remote would be a mistake the user cannot see they are making. Slow and right
// beats fast and wrong, and an empty list beats both.
//
// Bounded by the caller through completionDeadline. Reported as an error rather than as empty results, so a
// caller can tell "nothing matched" from "could not ask".
func (g *globals) completionServer(ctx context.Context) (transport.Conn, serverv1.ServerClient, error) {
	// noStart, because a tab press must not bring a server into being on another machine: completing a name
	// is a question, and starting a server is not part of asking it.
	d, err := g.remoteDialerFor(true)
	if err != nil {
		return nil, nil, err
	}
	if d != nil {
		return d.Dial(ctx)
	}

	dirs, err := g.dirs()
	if err != nil {
		return nil, nil, err
	}
	return dialServer(dirs)
}

// sshCommandLine returns the ssh command line to use, or nil for plain ssh.
//
// strings.Fields rather than a shell parse, which is a limit worth stating rather than hiding: `kitten ssh`
// and `ssh -F /etc/other` both work, and a path with a space in it does not. A shell parse would invite
// quoting bugs into an argv that reaches a process, for a case nobody has.
//
// A failure to read the config is ignored here, matching globals.dirs: this is reached from
// PersistentPreRunE to build a message, and a malformed file should be reported by the command that needs
// the setting rather than by every command that mentions a remote. remoteSettings is the one that reports it.
func (g *globals) sshCommandLine() []string {
	if g.sshCommand != "" {
		return strings.Fields(g.sshCommand)
	}
	if cfg, err := g.config(); err == nil && cfg != nil {
		return strings.Fields(cfg.Remote.SSHCommand)
	}
	return nil
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
	// Input and output, including the --follow forms, which attach the way `cm attach` does and so needed
	// the same dialer. See globals.pointAtServer.
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
	// The attachment itself, and the picker that hands a session to one. The terminal stays here and the
	// session is there, which is the whole point of --remote rather than a limitation of it.
	"attach": true,
	"tui":    true,
}

// remotePending lists the commands that will work against a remote but do not yet.
//
// Empty, and kept rather than deleted because it is the right home for the next one: a command that is
// remote-capable in principle and not yet wired belongs here rather than in machineLocal, which says
// something permanent, and rather than nowhere, which would let it act on the local server while the user
// asked about another. attach and tui lived here until they were wired.
//
// The value is the ssh options a suggested command needs, "-t" for anything interactive.
var remotePending = map[string][]string{}

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
			cmd.CommandPath(), t.SuggestionWith(g.sshCommandLine(), sshFlags, words...))
	}
	if machineLocal[name] {
		return fmt.Errorf(
			"%s is about the machine it runs on, so it cannot be pointed at a remote; run it there instead:\n"+
				"    %s",
			cmd.CommandPath(), t.Suggestion(g.sshCommandLine(), words...))
	}
	return nil
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
