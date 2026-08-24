// Package sessionenv decides which environment variables follow a client into a session, and
// renders them for a shell to apply.
//
// The problem it solves: a terminal emulator tells its child about itself through the
// environment, and those values are captured once when a session's shell starts. Reattaching
// from a new terminal, or from the same terminal after it restarted, leaves the shell holding
// values that describe a terminal that no longer exists. kitty's KITTY_LISTEN_ON is the sharp
// case, since every `kitten @` call goes through it and fails once the socket is gone.
//
// Nothing outside a process can change its environment, so the shell has to ask. cm records what
// the most recent client had and prints it on request; a shell hook applies it. This is the shape
// tmux settled on.
package sessionenv

import (
	"fmt"
	"sort"
	"strings"

	"github.com/chancez/cm/internal/paths"
)

// DefaultCapture lists the variables worth following a client into a session.
//
// Deliberately a list rather than the client's whole environment. A session record lives in a
// file on disk, and a developer's environment routinely holds API tokens and credentials that
// have no business being written there. Everything here is something a terminal or session
// manager sets to describe itself, and all of it goes stale when that terminal is replaced.
//
// A trailing "*" matches by prefix.
var DefaultCapture = []string{
	// What the terminal is and how to talk to it.
	"TERM",
	"COLORTERM",
	"TERMINFO",
	"TERM_PROGRAM",
	"TERM_PROGRAM_VERSION",
	"WINDOWID",

	// Per-emulator control channels and identifiers. KITTY_LISTEN_ON is the one that motivated
	// this: it names a socket that disappears when kitty restarts.
	"KITTY_*",
	"GHOSTTY_*",
	"ITERM_*",
	"ITERM_SESSION_ID",
	"WEZTERM_*",
	"ALACRITTY_*",
	"VTE_VERSION",
	"FOOT_*",

	// Display servers, which change when a session is reattached from elsewhere.
	"DISPLAY",
	"WAYLAND_DISPLAY",

	// The ssh agent socket and connection details. Not terminal state, but stale in exactly the
	// same way and with a worse symptom: a long-lived session loses the ability to push to git.
	"SSH_AUTH_SOCK",
	"SSH_CONNECTION",
	"SSH_CLIENT",
	"SSH_TTY",
}

// NoInherit lists the variables a session does *not* take from the client that created it, even
// though everything else is forwarded.
//
// A denylist rather than an allow-list, which is the opposite of DefaultCapture and deliberate. The
// two answer different questions. Capture decides what gets *recorded*, so it has to be an
// allow-list: a session record is a file on disk and a developer's environment holds credentials.
// This decides what a shell is *born with*, which is not recorded anywhere, and where the useful
// default is "what the creating client had" for the same reason a subshell inherits everything.
//
// Only the dynamic linker variables are excluded, and the reason is borrowed rather than invented.
// sshd defaults PermitUserEnvironment to no, and says why: it "may enable users to bypass access
// restrictions in some configurations using mechanisms such as LD_PRELOAD". These choose what code a
// process loads rather than how it behaves, which is the one category worth treating differently.
//
// The trust boundary sshd defends is absent here, since cm's client is a local process already
// running as the user, so this is closer to hygiene than to a security control. It is cheap and it
// has a precedent, where a broader denylist guessing at which names hold secrets would be neither:
// ARTIFACTORY_CLOUD_AUTH and HUBBLE_CLIENT_SECRET match a *_AUTH and *_SECRET pattern, and the next
// one would not.
//
// A trailing "*" matches by prefix.
var NoInherit = []string{
	"LD_PRELOAD",
	"LD_LIBRARY_PATH",
	"LD_AUDIT",
	// macOS spells them differently and has more of them, and DYLD_INSERT_LIBRARIES is the direct
	// LD_PRELOAD equivalent.
	"DYLD_*",
}

// Inherit returns the environment a newly created session takes from its client: everything the
// client has, less NoInherit.
//
// Applied once when the shim spawns the shell and never afterwards, which is the whole model. That
// is not a limitation to work around: a terminal split freezes its environment at creation too, and
// picking up a changed shell config by closing a window and opening a new one is the normal
// workflow. `cm get-env` covers the genuinely different case, where a terminal is replaced under a
// shell that is already running.
//
// Input is KEY=VALUE entries as os.Environ produces them, and order is preserved so a spawn is
// reproducible. Entries without '=' are skipped rather than guessed at.
//
// CM_SESSION needs no special case here even though a client inside a session has it: the shim
// appends its own after this, and exec keeps the last occurrence of a name, so a forwarded value
// cannot make a session claim to be its parent.
func Inherit(environ []string) []string {
	skip := NewMatcher(NoInherit)

	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if skip.Match(k) {
			continue
		}
		out = append(out, kv)
	}
	return out
}

// Matcher decides whether a variable should be captured.
type Matcher struct {
	exact    map[string]struct{}
	prefixes []string
}

// NewMatcher builds a matcher from patterns, where a trailing "*" matches by prefix.
func NewMatcher(patterns []string) *Matcher {
	m := &Matcher{exact: make(map[string]struct{}, len(patterns))}
	for _, p := range patterns {
		if strings.HasSuffix(p, "*") {
			m.prefixes = append(m.prefixes, strings.TrimSuffix(p, "*"))
			continue
		}
		m.exact[p] = struct{}{}
	}
	return m
}

// Match reports whether a variable name should be captured.
func (m *Matcher) Match(name string) bool {
	if _, ok := m.exact[name]; ok {
		return true
	}
	for _, p := range m.prefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// Capture selects the variables to record from a client's environment.
//
// Input is KEY=VALUE entries as os.Environ produces them. Entries without '=' are skipped rather
// than guessed at.
func Capture(environ []string, m *Matcher) map[string]string {
	out := make(map[string]string)
	for _, kv := range environ {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if m.Match(k) {
			out[k] = v
		}
	}
	return out
}

// ClientValues returns the names in environ that describe a client rather than the machine.
//
// For stripping them from a process that outlives any one client, which means the server. The capture
// list already decides which variables belong to a client and its terminal, so this reuses it rather
// than keeping a second list that could disagree with it. CM_SESSION is added on top, because it names
// one session, which is client state by definition.
//
// The incident: a server started from a shell inside an SSH session held SSH_CLIENT, SSH_CONNECTION and
// SSH_TTY. Every shim inherits the server's environment and has the creating client's values layered
// *over* it, so a name the client does not have is never overwritten and survives into the shell. Every
// session created afterwards looked like an SSH login, and prompts printed user@host, including in
// splits of sessions that had never been near SSH. SSH_AUTH_SOCK in the same session was correct, which
// is the tell: the client had one to overwrite it with, and no local client has an SSH_CLIENT.
//
// It also survived every server restart, because a restart is spawned from a shell that has the values
// by then, and reinstalling the previous binary changed nothing, since the running process is what
// carries them.
//
// Names are returned sorted and deduplicated, so a caller can log them and a test can assert on them.
func ClientValues(environ []string, m *Matcher) []string {
	seen := make(map[string]struct{}, len(environ))
	var names []string
	for _, kv := range environ {
		k, _, ok := strings.Cut(kv, "=")
		if !ok || k == "" {
			continue
		}
		if !m.Match(k) && k != paths.SessionEnv() {
			continue
		}
		if _, dup := seen[k]; dup {
			continue
		}
		seen[k] = struct{}{}
		names = append(names, k)
	}
	sort.Strings(names)
	return names
}

// Format selects how variables are rendered.
type Format int

const (
	// FormatPlain prints KEY=VALUE lines, with removals as "-KEY".
	//
	// The default because it is unambiguous and parseable, and because emitting shell code by
	// default would pick a shell for the user. tmux always emits POSIX, which is broken in fish
	// on every count: a bare assignment is a per-command prefix there, export is not a builtin,
	// and unset is `set -e`.
	FormatPlain Format = iota
	// FormatPosix prints code for sh, bash, and zsh to eval.
	FormatPosix
	// FormatFish prints code for fish to eval.
	FormatFish
)

// ParseFormat resolves a format name.
//
// Shell detection is deliberately absent. $SHELL is the user's login shell, not the shell
// running inside the session, and those differ whenever someone starts a different one, which is
// exactly when guessing wrong is most confusing.
func ParseFormat(name string) (Format, error) {
	switch name {
	case "", "plain":
		return FormatPlain, nil
	case "posix", "sh", "bash", "zsh":
		return FormatPosix, nil
	case "fish":
		return FormatFish, nil
	default:
		return FormatPlain, fmt.Errorf("unknown format %q, want plain, posix, or fish", name)
	}
}

// Diff describes how a session's recorded environment differs from what a shell currently has.
type Diff struct {
	// Set holds variables to assign.
	Set map[string]string
	// Unset names variables the client no longer has.
	//
	// Tracking removals matters as much as tracking values. A variable that vanished, rather than
	// changed, keeps its old value if only assignments are emitted, and a stale socket path is
	// worse than an absent one: a client tries to connect and fails instead of falling back.
	Unset []string
}

// Compute compares a session's recorded environment against a shell's current one.
//
// known is the set of names cm manages, so a variable absent from recorded but present in current
// is reported for unsetting only if cm would have captured it. Without that, every unrelated
// variable in the shell would look like a removal.
func Compute(recorded, current map[string]string, m *Matcher) Diff {
	d := Diff{Set: make(map[string]string)}

	for k, v := range recorded {
		if cur, ok := current[k]; !ok || cur != v {
			d.Set[k] = v
		}
	}
	for k := range current {
		if !m.Match(k) {
			continue
		}
		if _, ok := recorded[k]; !ok {
			d.Unset = append(d.Unset, k)
		}
	}
	sort.Strings(d.Unset)
	return d
}

// Render writes a diff in the requested format.
func Render(d Diff, f Format) string {
	names := make([]string, 0, len(d.Set))
	for k := range d.Set {
		names = append(names, k)
	}
	sort.Strings(names)

	var b strings.Builder
	switch f {
	case FormatPosix:
		for _, k := range names {
			fmt.Fprintf(&b, "export %s=%s\n", k, posixQuote(d.Set[k]))
		}
		for _, k := range d.Unset {
			fmt.Fprintf(&b, "unset %s\n", k)
		}
	case FormatFish:
		for _, k := range names {
			fmt.Fprintf(&b, "set -gx %s %s\n", k, fishQuote(d.Set[k]))
		}
		for _, k := range d.Unset {
			fmt.Fprintf(&b, "set -e %s\n", k)
		}
	default:
		for _, k := range names {
			fmt.Fprintf(&b, "%s=%s\n", k, d.Set[k])
		}
		// A leading '-' marks a removal, which is how tmux distinguishes them and keeps the
		// output one value per line.
		for _, k := range d.Unset {
			fmt.Fprintf(&b, "-%s\n", k)
		}
	}
	return b.String()
}

// posixQuote wraps a value in single quotes for sh, bash, and zsh.
//
// Single quotes are literal in all three, so the only character needing care is the quote itself,
// which is closed, escaped, and reopened. Values here are socket paths and identifiers, but they
// come from another process's environment and are not trustworthy on that basis alone.
func posixQuote(v string) string {
	return "'" + strings.ReplaceAll(v, "'", `'\''`) + "'"
}

// fishQuote wraps a value in single quotes for fish, where backslash and single quote are the two
// characters that remain special inside them.
func fishQuote(v string) string {
	v = strings.ReplaceAll(v, `\`, `\\`)
	v = strings.ReplaceAll(v, "'", `\'`)
	return "'" + v + "'"
}
