package paths

import (
	"errors"
	"fmt"
	"strings"
)

// MaxSessionNameLen bounds a name for readability and to leave room in a socket path.
// The real constraint on the path is checked by CheckSocketPath, since it depends on the
// runtime directory too.
const MaxSessionNameLen = 64

// MaxSocketPathLen is the longest usable unix socket path.
//
// sockaddr_un.sun_path is 104 bytes on darwin and 108 on linux, including the terminating
// NUL, so 103 is safe on both. Exceeding it fails at bind() with EINVAL, which surfaces as
// an opaque "invalid argument" rather than anything suggesting the path is too long.
const MaxSocketPathLen = 103

// CheckSocketPath reports whether a socket path fits in sockaddr_un.
//
// This is worth checking explicitly because the failure is otherwise baffling: a long
// TMPDIR, which is common on macOS where per-user temp directories are deep, turns a
// valid session name into a bind error that names neither the length nor the limit.
func CheckSocketPath(path string) error {
	if len(path) > MaxSocketPathLen {
		return fmt.Errorf(
			"socket path %q is %d bytes, over the %d-byte limit for unix sockets; "+
				"use a shorter session name or set %s to a shorter directory",
			path, len(path), MaxSocketPathLen, Env("RUNTIME_DIR"))
	}
	return nil
}

// ErrEmptySessionName is returned for an empty name.
var ErrEmptySessionName = errors.New("session name is empty")

// ValidateSessionName reports whether a name may be bound to a session.
//
// Names no longer become filenames: a session's shim socket and output log are named after its ID, so
// the path traversal boundary this used to be is now ValidateSessionID's. What remains is still worth
// enforcing. A name is printed by `cm list`, so a name carrying an escape sequence could repaint or
// retitle the terminal of whoever ran it, which is the same argument tag keys and values are checked
// under. It also has to stay usable unquoted in a shell, since every command takes one.
//
// The allowed set is letters, digits, '-', '_', and '.', with '.' forbidden as the
// first character. That rejects "." and ".." without special-casing them, and it excludes '@', which
// is what makes an ID reference impossible to confuse with a name. See IDSigil.
func ValidateSessionName(name string) error {
	if name == "" {
		return ErrEmptySessionName
	}
	if len(name) > MaxSessionNameLen {
		return fmt.Errorf("session name %q is %d bytes, limit is %d",
			name, len(name), MaxSessionNameLen)
	}
	// Checking for a leading '.' covers "." and ".." and also avoids creating hidden
	// files, which would make sockets and logs easy to miss when debugging.
	if strings.HasPrefix(name, ".") {
		return fmt.Errorf("session name %q starts with '.'", name)
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return fmt.Errorf("session name %q contains disallowed character %q", name, r)
		}
	}
	return nil
}

// IDSigil marks a reference as a session ID rather than a name.
//
// '@' is not in the set ValidateSessionName allows, so a reference starting with it is unambiguously an
// ID and no name can ever be taken for one. That is a proof rather than a convention, and it is the
// reason the sigil exists: without it `cm attach 7` would become ambiguous the moment somebody bound
// the name 7, and the ambiguity would resolve differently depending on which sessions happened to
// exist.
const IDSigil = "@"

// MaxSessionIDLen bounds an ID, for the same socket-path reason MaxSessionNameLen bounds a name.
//
// Generated IDs are 8 characters. The limit is well above that so the ones the ID migration backfilled
// still pass, and so a longer format later is not a validation change.
const MaxSessionIDLen = 32

// ErrEmptySessionID is returned for an empty ID.
var ErrEmptySessionID = errors.New("session ID is empty")

// SessionRef splits a reference typed by a user into a value and what kind of thing it names.
//
// One place rather than a strings.TrimPrefix at every call site, because a command that forgot to strip
// the sigil would look up a name of "@a7k2m9x4", find nothing, and create a session under that name
// instead of attaching to the one the user asked for.
func SessionRef(ref string) (value string, isID bool) {
	if rest, found := strings.CutPrefix(ref, IDSigil); found {
		return rest, true
	}
	return ref, false
}

// FormatSessionID renders an ID the way a user types it back.
func FormatSessionID(id string) string { return IDSigil + id }

// ValidateSessionID reports whether an ID is safe to use in a path.
//
// This is the path traversal boundary that ValidateSessionName used to be: an ID names both the shim
// socket and the output log, so an ID containing a separator could place a socket outside the runtime
// directory, and stale-socket cleanup could then unlink an arbitrary file. IDs are generated rather
// than supplied, so this should never fire on anything cm produced, and it is checked anyway because
// the value arrives from a database file and from a command line, neither of which this process wrote.
//
// Lowercase letters and digits only, which is stricter than the name rules: the generator draws from a
// subset of exactly that, and every future format has room inside it.
func ValidateSessionID(id string) error {
	if id == "" {
		return ErrEmptySessionID
	}
	if len(id) > MaxSessionIDLen {
		return fmt.Errorf("session ID %q is %d bytes, limit is %d", id, len(id), MaxSessionIDLen)
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
		default:
			return fmt.Errorf("session ID %q contains disallowed character %q", id, r)
		}
	}
	return nil
}
