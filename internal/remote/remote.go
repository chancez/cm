// Package remote turns a reference to a cm server on another machine into the command that reaches it.
//
// Nothing here connects anything: it decides what to run, and internal/transport runs it. The split is so
// that the argv is a value a test can assert on, which matters because half of what this package knows is
// about failure modes that only appear on a real link.
package remote

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/chancez/cm/internal/paths"
)

// Scheme is the only transport cm speaks to another machine over.
//
// Only ssh, and that is a decision rather than a first step. cm has no authentication of its own, so a
// tunnel that borrows ssh's is the whole reason remote access is possible without building one: see
// docs/rpc.md, and "A cm that listens on the network" in docs/ideas.md for the thing this is not.
const Scheme = "ssh"

// DefaultCommand is the cm to run on the remote when the reference does not name one.
//
// Bare, so the remote's PATH resolves it, which is what makes `--remote ssh://host` work with no setup on
// a host where cm is installed normally.
const DefaultCommand = paths.Name

// Target is a cm server on another machine.
type Target struct {
	// User is the login to use, empty for whatever ssh itself would choose.
	User string
	// Host is the name to connect to, which may be an ssh alias rather than a hostname. Keeping the alias
	// is deliberate: it is what the user typed, what their ssh config knows, and what a listing should
	// show, and resolving it here would throw all three away.
	Host string
	// Port is the port to connect to, 0 for ssh's own default. Also possibly set by the user's ssh config,
	// which is why 0 means "do not pass one" rather than 22.
	Port int
	// Command is the cm binary to run on the remote. Empty means DefaultCommand.
	//
	// Worth having at all because a non-interactive ssh gets a different PATH than a login shell, so a cm
	// installed in ~/.local/bin or /usr/local/bin is routinely not found by `ssh host cm`. Without a way to
	// say where it is, remote access would be unusable on those hosts for a reason that looks like a cm bug.
	Command string
}

// Parse reads a reference to a remote cm server.
//
// Accepts ssh://[user@]host[:port][/path/to/cm] and the bare [user@]host[:port] that ssh itself takes. The
// bare form is accepted because an ssh alias is what a user types everywhere else, and requiring a scheme
// for the same thing is friction with nothing behind it. Anything carrying a scheme must carry a known one,
// so a typo is a refusal rather than a hostname nobody can resolve.
func Parse(s string) (Target, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return Target{}, fmt.Errorf("no remote given")
	}

	if scheme, _, ok := strings.Cut(s, "://"); ok {
		if scheme != Scheme {
			return Target{}, fmt.Errorf("%q is not a cm remote: only %s:// is supported", s, Scheme)
		}
		return parseURL(s)
	}
	return parseHostPort(s, "")
}

// parseURL reads the ssh:// form.
func parseURL(s string) (Target, error) {
	u, err := url.Parse(s)
	if err != nil {
		return Target{}, fmt.Errorf("%q is not a valid %s URL: %w", s, Scheme, err)
	}
	// Rejected rather than ignored, because a password in a cm command line would be recorded by every
	// shell history and shown by ps, and ssh would not use it anyway.
	if _, hasPassword := u.User.Password(); hasPassword {
		return Target{}, fmt.Errorf("a password in a remote is not supported; use an ssh key")
	}

	// The path verbatim, leading slash included, because that is the whole point of naming one: an absolute
	// path is how a cm that a non-interactive ssh's PATH does not find gets run. Trimming the slash made it
	// relative to the login directory, which either runs nothing or runs the wrong thing.
	t, err := parseHostPort(u.Host, u.Path)
	if err != nil {
		return Target{}, err
	}
	if u.User != nil {
		t.User = u.User.Username()
	}
	return t, nil
}

// parseHostPort reads [user@]host[:port], which is the form ssh itself accepts.
func parseHostPort(s, command string) (Target, error) {
	t := Target{Command: command}

	if user, rest, ok := strings.Cut(s, "@"); ok {
		if user == "" {
			return Target{}, fmt.Errorf("a remote cannot start with @")
		}
		t.User = user
		s = rest
	}

	if host, port, ok := strings.Cut(s, ":"); ok {
		n, err := strconv.Atoi(port)
		if err != nil || n <= 0 || n > 65535 {
			return Target{}, fmt.Errorf("%q is not a valid port", port)
		}
		t.Host = host
		t.Port = n
	} else {
		t.Host = s
	}

	if t.Host == "" {
		return Target{}, fmt.Errorf("a remote needs a host")
	}
	return t, nil
}

// String renders the target as the reference that produced it.
//
// Round-trips through Parse, so it is safe to put in a re-exec's argv, which `cm switch` needs: a switch
// replaces the client with `cm attach @<id>`, and a remote client has to stay pointed at the same host.
func (t Target) String() string {
	var sb strings.Builder
	sb.WriteString(Scheme + "://")
	if t.User != "" {
		sb.WriteString(t.User + "@")
	}
	sb.WriteString(t.Host)
	if t.Port != 0 {
		fmt.Fprintf(&sb, ":%d", t.Port)
	}
	if t.Command != "" {
		if !strings.HasPrefix(t.Command, "/") {
			sb.WriteString("/")
		}
		sb.WriteString(t.Command)
	}
	return sb.String()
}

// DefaultControlPersist is how long a shared ssh connection outlives the command that opened it.
//
// The number is a trade with two visible ends. Measured on loopback, a fresh connection costs 120 to 150ms
// and one through an existing master costs 10 to 30ms, so anything that expires between two cm commands
// pays the full price twice; and a master that lingers is an ssh process the user did not start, holding a
// connection to a host they may have finished with.
//
// A minute covers a burst of commands, which is how cm is actually used -- a list, a send, a read, a kill --
// while being short enough that an idle laptop is not holding connections open to somewhere.
const DefaultControlPersist = 60 * time.Second

// ControlPath returns where a shared connection to this target keeps its control socket.
//
// Empty when multiplexing cannot be used, which callers treat as "do not ask for it" rather than as an
// error: a slow connection is worse than a fast one and better than a failure.
//
// cm computes the name rather than using ssh's own %C token, and that is the difference between having this
// feature and not. %C is a 64-character hash, which under a runtime directory already 55 bytes on macOS
// makes a 124-byte socket path against the 103 a unix socket allows. Eight bytes of the same information
// fit: 68 bytes, measured. The hash covers everything that decides which connection this is, so two
// targets differing only in port do not share one.
func (t Target) ControlPath(dir string) string {
	if dir == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%s\x00%d", t.User, t.Host, t.Port)))
	path := filepath.Join(dir, "ssh-"+hex.EncodeToString(sum[:4]))
	// Checked rather than assumed, because a deep TMPDIR can still overflow it and the failure would be an
	// opaque EINVAL from ssh rather than anything naming a length. paths.MaxSocketPathLen holds the limit.
	if paths.CheckSocketPath(path) != nil {
		return ""
	}
	return path
}

// Dialing is how cm reaches a remote, as distinct from which remote it is.
type Dialing struct {
	// Command is the ssh command line to run, empty for plain ssh.
	//
	// A command line rather than a program, because the useful overrides are several words: `kitten ssh`,
	// or an `ssh -F` naming another config. Split on whitespace by whoever supplies it; there is no shell
	// quoting, which is worth knowing before putting a path with a space in it here.
	//
	// Whatever it is receives cm's own ssh options, so it has to accept them and has to pass bytes through
	// unaltered. A wrapper that allocates a pty corrupts the protocol rather than failing, which is why the
	// proxy opens with a banner: a mangled stream is reported as "expected a cm proxy, got ..." on the first
	// connection instead of as a strange session later.
	Command []string
	// ControlDir is where a shared connection keeps its control socket, empty for a connection of this
	// command's own. Sharing is what makes a remote usable rather than merely possible: every cm command is
	// one ssh, and without it each pays a fresh connection.
	ControlDir string
	// ControlPersist is how long that connection outlives the command, zero or less for no sharing at all.
	//
	// No default applied here, so this package holds no policy a caller cannot see: DefaultControlPersist is
	// the value to pass, and the config file is where it is chosen. Zero meaning "do not share" is what lets
	// one setting cover both a host where ControlMaster is unwelcome and a host where a minute is too short.
	ControlPersist time.Duration
	// Start asks the remote to bring a server up if none is running. See `cm server proxy` for why that is
	// the caller's decision rather than something the far end always does.
	Start bool
}

// program returns the command to run and any arguments that belong to it.
func (d Dialing) program() (string, []string) {
	if len(d.Command) == 0 {
		return Scheme, nil
	}
	return d.Command[0], d.Command[1:]
}

// SSHHost is how ssh is told which host to reach, as user@host or bare host.
func (t Target) SSHHost() string {
	if t.User != "" {
		return t.User + "@" + t.Host
	}
	return t.Host
}

// ProxyCommand returns the program and arguments that connect to this target's cm server.
func (t Target) ProxyCommand(d Dialing) (string, []string) {
	name, args := d.program()
	args = append(args,
		// No pty, and said here rather than relied on. A command over ssh gets none by default, but
		// RequestTTY in a user's config overrides that default, and a pty would translate this protocol's
		// bytes: \n becomes \r\n on the way through a terminal line discipline, which corrupts every
		// message rather than failing cleanly.
		"-T",
		// No escape character, for a reason that is rare and catastrophic rather than likely. ssh watches
		// for ~ after a newline and acts on what follows, and this stream is arbitrary binary, so a message
		// containing "\n~." would kill the connection from inside. Escapes are inactive without a pty,
		// which -T already ensures, so this is the second lock on the same door.
		"-e", "none",
		// Non-interactive, because a prompt from ssh is a disaster in this position rather than a
		// convenience. A passphrase or password prompt reads and writes /dev/tty directly, bypassing the
		// pipes and landing on the terminal an attached client is painting. Failing instead is recoverable:
		// the message says what happened, and one `ssh host true` by hand unlocks the key for the agent.
		"-o", "BatchMode=yes",
		// So a link that goes away is an error rather than a hang. Without this a connection dropped by a
		// NAT or a sleeping laptop stays open as far as this end is concerned, and the client sits in an
		// attachment that looks live and answers nothing. With it, ssh gives up after three missed probes
		// and the client's reconnect loop takes over, which is the behavior a server restart already has.
		"-o", "ServerAliveInterval=15",
		"-o", "ServerAliveCountMax=3",
	)
	// One shared connection per target, reused by every cm command and kept for a while after the last one.
	// Every cm command against a remote is an ssh, so without this each pays a fresh connection: 120 to
	// 150ms against 10 to 30ms, measured on loopback, where a real host's handshake is slower still.
	//
	// ControlMaster=auto rather than yes, so whichever command runs first becomes the master and the rest
	// attach to it, with no ordering to arrange and nothing to clean up: the socket lives in cm's runtime
	// directory, which is swept with the rest of it.
	//
	// This overrides a ControlPath the user's own ssh config may set, which means cm keeps its own master
	// rather than joining theirs. Deliberate: finding theirs means parsing their config or paying an `ssh -G`
	// on every invocation, and a second master for one host costs a process, while guessing wrong costs
	// correctness. Whoever wants only theirs can pass an empty controlDir.
	if path := t.ControlPath(d.ControlDir); path != "" && d.ControlPersist > 0 {
		args = append(args,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+path,
			"-o", fmt.Sprintf("ControlPersist=%d", int(d.ControlPersist.Seconds())),
		)
	}

	if t.Port != 0 {
		args = append(args, "-p", strconv.Itoa(t.Port))
	}

	args = append(args, t.SSHHost())

	// After a --, so a remote whose command needs no quoting cannot be read as more ssh options.
	args = append(args, "--", t.command(), "server", "proxy")
	if d.Start {
		args = append(args, "--start")
	}
	return name, args
}

// Suggestion renders the ssh command a user would type to run cm on this target themselves.
//
// For the commands that refuse to act on another machine, where the refusal is only useful if it says how
// to get the answer. Built here rather than formatted at the call site so it stays right for a target with
// a port or a cm somewhere unusual, which is exactly when a user cannot guess it.
func (t Target) Suggestion(command []string, args ...string) string {
	return t.SuggestionWith(command, nil, args...)
}

// SuggestionWith is Suggestion with extra ssh options, for a command that needs something of ssh itself.
//
// -t is the one that matters: an interactive cm on the far end needs a pty, and ssh allocates none for a
// command, so a suggested `ssh host cm attach` without it would fail in a way that looks like cm's fault.
func (t Target) SuggestionWith(command, sshFlags []string, args ...string) string {
	if len(command) == 0 {
		command = []string{Scheme}
	}
	parts := append(append([]string{}, command...), sshFlags...)
	if t.Port != 0 {
		parts = append(parts, "-p", strconv.Itoa(t.Port))
	}
	parts = append(parts, t.SSHHost())
	parts = append(parts, t.command())
	return strings.Join(append(parts, args...), " ")
}

// command is the cm to run on the remote.
func (t Target) command() string {
	if t.Command == "" {
		return DefaultCommand
	}
	return t.Command
}
