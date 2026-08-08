// Package paths resolves the runtime, state, and log locations cm uses, and is the
// single place the binary's name appears. Renaming the project means editing Name here
// rather than hunting through socket paths, env var prefixes, and state directories.
package paths

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Name is the binary name, and the stem of every path and env var derived from it.
const Name = "cm"

// EnvPrefix prefixes every environment variable cm reads or exports, e.g. CM_SESSION.
var EnvPrefix = strings.ToUpper(Name) + "_"

// Env returns the name of a cm environment variable, so callers spell the suffix only.
func Env(suffix string) string {
	return EnvPrefix + suffix
}

// SessionEnv names the variable exported into a session's shell, holding its session
// name. Its presence is how a process knows it is running inside cm.
func SessionEnv() string {
	return Env("SESSION")
}

// Dirs holds the resolved directories for a cm instance. Keeping them in one struct
// lets tests point an entire instance at a temporary directory.
type Dirs struct {
	// Runtime holds sockets: the server's, and one per shim. It doubles as a registry
	// of shims that the server can discover without consulting the database.
	Runtime string
	// State holds the sqlite database and per-session output logs. Unlike Runtime it is
	// meant to survive a reboot.
	State string
}

// Default resolves the directories from the environment.
//
// Runtime prefers XDG_RUNTIME_DIR, falling back to a uid-qualified directory under
// TMPDIR. The uid qualifier matters: a shared /tmp path would collide between users and
// let one user's socket shadow another's.
//
// Resolution is deliberately narrow. zmx supports a chain of four locations and the
// result is that a process started without TMPDIR set lands somewhere its own `list`
// cannot see. A single override is easier to reason about.
func Default() (Dirs, error) {
	var d Dirs

	if root := os.Getenv(Env("RUNTIME_DIR")); root != "" {
		d.Runtime = root
	} else if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		d.Runtime = filepath.Join(xdg, Name)
	} else {
		tmp := os.TempDir()
		d.Runtime = filepath.Join(tmp, fmt.Sprintf("%s-%d", Name, os.Getuid()))
	}

	if root := os.Getenv(Env("STATE_DIR")); root != "" {
		d.State = root
	} else if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		d.State = filepath.Join(xdg, Name)
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return Dirs{}, fmt.Errorf("resolving home directory: %w", err)
		}
		d.State = filepath.Join(home, ".local", "state", Name)
	}

	return d, nil
}

// Ensure creates the directories, owner-only. The sockets inside grant control of a
// shell, so the permissions are the access control.
func (d Dirs) Ensure() error {
	for _, dir := range []string{d.Runtime, d.State, d.logDir()} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}
	return nil
}

// ServerSocket is the path clients connect to.
func (d Dirs) ServerSocket() string {
	return filepath.Join(d.Runtime, "server.sock")
}

// ShimSocket is the path the server uses to reach one session's shim.
//
// Callers must validate the name first: it becomes a filename, so an unchecked name
// could place a socket outside Runtime, or make stale-socket cleanup delete an
// arbitrary file. See ValidateSessionName.
func (d Dirs) ShimSocket(session string) string {
	return filepath.Join(d.Runtime, "shim-"+session+".sock")
}

// Database is the sqlite file holding session metadata.
func (d Dirs) Database() string {
	return filepath.Join(d.State, Name+".db")
}

func (d Dirs) logDir() string {
	return filepath.Join(d.State, "logs")
}

// SessionLog is the append-only output log for one session. Terminal output goes here
// rather than into sqlite: it is high-volume, written sequentially, and only ever read
// back in order.
func (d Dirs) SessionLog(session string) string {
	return filepath.Join(d.logDir(), session+".log")
}
