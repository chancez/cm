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
