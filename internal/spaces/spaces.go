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
	"cmp"
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
//
// A leading '.' is allowed, unlike in a session name, where it kept a socket from being a hidden file. A
// space is never a file, and kspaces names a space after its repository's directory, so ~/.dotfiles is
// .dotfiles: refusing it put every window there in default. "." and ".." are still refused, as names
// that read as directories rather than as spaces.
func ValidateName(name string) error {
	if name == "" {
		return ErrEmptyName
	}
	if len(name) > MaxNameLen {
		return fmt.Errorf("space name %q is %d bytes, limit is %d", name, len(name), MaxNameLen)
	}
	if name == "." || name == ".." {
		return fmt.Errorf("space name %q is not a name", name)
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

// Compare orders spaces as listings group by them: default first, since it is where everything without a
// space of its own lands, then by name. Empty, from a server that predates spaces, sorts as default.
func Compare(a, b string) int {
	a, b = cmp.Or(a, Default), cmp.Or(b, Default)
	switch {
	case a == b:
		return 0
	case a == Default:
		return -1
	case b == Default:
		return 1
	}
	return strings.Compare(a, b)
}
