package paths

import (
	"errors"
	"fmt"
	"strings"
)

// MaxSessionNameLen bounds a name so the resulting socket path stays inside the
// sockaddr_un limit, which is 104 bytes on darwin and 108 on linux. The bound is
// conservative rather than computed: it leaves room for the runtime directory, the
// "shim-" prefix, and the ".sock" suffix.
const MaxSessionNameLen = 64

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
