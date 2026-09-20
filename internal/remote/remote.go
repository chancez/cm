// Package remote turns a reference to a cm server on another machine into the command that reaches it.
//
// Nothing here connects anything: it decides what to run, and internal/transport runs it. The split is so
// that the argv is a value a test can assert on, which matters because half of what this package knows is
// about failure modes that only appear on a real link.
package remote

import (
	"fmt"
	"net/url"
	"strconv"
	"strings"

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

// ProxyCommand returns the program and arguments that connect to this target's cm server.
//
// start asks the remote to bring a server up if none is running. See `cm server proxy` for why that is a
// decision the caller makes rather than something the far end always does.
func (t Target) ProxyCommand(start bool) (string, []string) {
	args := []string{
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
	}
	// Deliberately no ControlMaster or ControlPath. Multiplexing is worth a lot here, measured at 137ms for
	// a fresh connection against 10 to 20ms through an existing one, but it belongs in the user's ssh config
	// rather than in cm's argv: a ControlPath of cm's own would have to live somewhere, and under the
	// runtime directory it does not fit. ssh's %C is a 64-character hash, and this machine's runtime
	// directory is already 55 bytes, so the socket would be 124 bytes against the 103 a unix socket allows.
	// paths.MaxSocketPathLen is that limit and the failure is a bare EINVAL.

	if t.Port != 0 {
		args = append(args, "-p", strconv.Itoa(t.Port))
	}

	host := t.Host
	if t.User != "" {
		host = t.User + "@" + t.Host
	}
	args = append(args, host)

	// After a --, so a remote whose command needs no quoting cannot be read as more ssh options.
	args = append(args, "--", t.command(), "server", "proxy")
	if start {
		args = append(args, "--start")
	}
	return Scheme, args
}

// command is the cm to run on the remote.
func (t Target) command() string {
	if t.Command == "" {
		return DefaultCommand
	}
	return t.Command
}
