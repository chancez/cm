// Package spaces holds the vocabulary for a space: the named group every session belongs to exactly one
// of.
//
// A space exists because a flat list of sessions stops being navigable somewhere in the tens, and tags do
// not fix that: a session can carry several or none, so "where is it" has no single answer, and a tag
// nothing carries does not exist. A space is one answer, and it outlives its sessions, so a long-running
// session whose window was closed is found again by the space it was left in. See docs/ideas.md, "Spaces,
// and sessions across hosts".
package spaces

import (
	"errors"
	"fmt"
	"strings"
)

// Default is the space a session is in when it was created without one. It always exists and cannot be
// removed, so no session is ever left without a space.
const Default = "default"

// MaxNameLen bounds a space name, matching a session name's limit.
const MaxNameLen = 64

// ErrEmptyName is returned for an empty space name.
var ErrEmptyName = errors.New("space name is empty")

// ValidateName checks a space name.
//
// The rules are a session name's, for its reasons: a space is printed by `cm list` and the sidebar, so
// one carrying an escape sequence could repaint the terminal of whoever lists it, and it is typed as a
// bare shell word. ':' is excluded for a reason of its own, since a qualified reference spells a host
// before one.
func ValidateName(name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("space name %q is %d bytes, limit is %d", name, len(name), MaxNameLen)
	}
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("space name %q starts with '.'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("space name %q contains disallowed character %q", name, r)
		}
	}
	return nil
}
