// Package osc interprets the OSC sequences a shell uses to report about itself.
//
// Terminal emulators need this: a new window should open in the session's directory, and a tab
// should show the session's title. The values arrive as escape sequences that libghostty stores
// verbatim, so decoding them is the embedder's job.
package osc

import (
	"net/url"
	"os"
	"strings"
)

// Cwd is a decoded working directory report.
type Cwd struct {
	// Path is the filesystem path, percent-decoded.
	Path string
	// Host is the hostname the shell reported, empty when it did not.
	Host string
	// IsLocal reports whether Path refers to this machine.
	//
	// This matters more than it looks. A session that has ssh'd elsewhere reports a path that
	// exists on the remote host, so acting on it locally, such as opening a new window there,
	// silently uses the wrong directory or fails. zmx tracks the same distinction.
	IsLocal bool
}

// ParseCwd decodes a directory report from OSC 7, OSC 9, or OSC 1337.
//
// OSC 7 sends a file:// URI; the others typically send a bare path. Both shapes are accepted
// because a shell's choice is not something the caller controls.
//
// Returns ok=false for input that carries no usable path, including the empty report a shell
// sends to clear the value.
func ParseCwd(raw string) (Cwd, bool) {
	// libghostty appends a NUL sentinel to the stored value. Forwarding it to a client is a
	// real bug rather than cosmetic: kitty writes it into its session file and then cannot
	// parse its own state back. See zmx issue 222.
	raw = strings.TrimRight(raw, "\x00")
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return Cwd{}, false
	}

	// A bare path, which is what OSC 9 and OSC 1337 usually send.
	if strings.HasPrefix(raw, "/") {
		return Cwd{Path: raw, IsLocal: true}, true
	}

	u, err := url.Parse(raw)
	if err != nil || u.Path == "" {
		return Cwd{}, false
	}
	// kitty sends kitty-shell-cwd:// in place of file:// when its shell integration is active,
	// so the scheme is not checked against a fixed value.
	path, err := url.PathUnescape(u.Path)
	if err != nil {
		// Undecodable escaping means the value cannot be trusted as a path.
		return Cwd{}, false
	}

	host := u.Hostname()
	return Cwd{Path: path, Host: host, IsLocal: isLocalHost(host)}, true
}

// isLocalHost reports whether a hostname refers to this machine.
//
// An empty host means the shell did not say, which in practice means local. Comparison falls
// back to the first label of each name, because one side is often a bare hostname while the
// other is fully qualified, and treating that as remote would wrongly mark ordinary local
// sessions as unusable.
func isLocalHost(host string) bool {
	if host == "" || host == "localhost" {
		return true
	}

	self, err := os.Hostname()
	if err != nil {
		// Without a local name to compare against, assume local: a false remote reading breaks
		// working directory inheritance, while a false local reading only affects sessions that
		// have actually ssh'd away.
		return true
	}

	if strings.EqualFold(host, self) {
		return true
	}
	return strings.EqualFold(firstLabel(host), firstLabel(self))
}

// firstLabel returns the portion of a hostname before the first dot.
func firstLabel(host string) string {
	if i := strings.IndexByte(host, '.'); i >= 0 {
		return host[:i]
	}
	return host
}
