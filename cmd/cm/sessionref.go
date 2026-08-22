package main

import (
	"context"
	"fmt"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
)

// resolveSessionID turns a reference into the ID of the session it names.
//
// Only for the commands that build a *path* out of a session, which is why it reads the database rather
// than asking the server: a shim's diagnostic log is named after the ID, and the reason to read one is
// usually that something is wrong, which is the worst time to require a running server. Everything else
// sends the reference to the server and lets it resolve, since the server holds the same table and is
// already being talked to.
//
// The store is opened read-only in effect: this only reads, and a missing database means no sessions
// exist, which is reported as the reference not resolving rather than as a failure to open a file the
// user has never heard of.
func resolveSessionID(ctx context.Context, dirs paths.Dirs, ref string) (string, error) {
	value, isID := paths.SessionRef(ref)
	if isID {
		if err := paths.ValidateSessionID(value); err != nil {
			return "", err
		}
		return value, nil
	}
	if err := paths.ValidateSessionName(value); err != nil {
		return "", err
	}

	st, err := store.Open(ctx, dirs.Database())
	if err != nil {
		return "", fmt.Errorf("reading session names: %w", err)
	}
	defer st.Close()

	binding, err := st.Binding(ctx, value)
	if err != nil {
		return "", fmt.Errorf("no session is named %q", value)
	}
	return binding.SessionID, nil
}

// sessionLogStem is resolveSessionID for building a log path, falling back to the reference itself.
//
// A reference that resolves to nothing is not an error here, and that is the existing contract rather
// than a shortcut: `cm logs shim x` on a session that never logged prints nothing and exits 0, and
// `--clear` on it is satisfied already. Resolving would have turned a name nothing holds into a failure,
// so `cm logs shim --clear neverexisted` started exiting 1 at the thing it is documented to allow.
//
// Falling back to the reference also keeps the message useful: it names a path under the name the user
// typed, which is what they would go looking for.
func sessionLogStem(ctx context.Context, dirs paths.Dirs, ref string) (string, error) {
	id, err := resolveSessionID(ctx, dirs, ref)
	if err == nil {
		return id, nil
	}
	// A malformed reference is still an error: it cannot name anything, and it would be used to build a
	// path.
	if value, isID := paths.SessionRef(ref); isID {
		return "", err
	} else if verr := paths.ValidateSessionName(value); verr != nil {
		return "", verr
	}
	return ref, nil
}
