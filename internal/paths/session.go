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

// ValidateSessionName reports whether a name is safe to use as a session identifier.
//
// Names become filenames, for both the shim socket and the output log, so this is a
// path traversal boundary and not merely a style check. Without it, a name containing
// a separator could place a socket outside the runtime directory, and stale-socket
// cleanup could unlink an arbitrary file.
//
// The allowed set is letters, digits, '-', '_', and '.', with '.' forbidden as the
// first character. That rejects "." and ".." without special-casing them, and keeps
// names usable unquoted in a shell.
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
