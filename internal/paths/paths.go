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

// DirOrigin names which rule resolved a directory, for reporting.
//
// Recorded because it cannot be worked out afterwards: every branch below produces an absolute path, so a
// value that came from XDG_STATE_HOME and one that came from the built-in default are indistinguishable once
// resolved. `cm config` reports where each setting came from, and without this it reported "default" for a
// path XDG had chosen.
type DirOrigin struct {
	// Runtime and State name the source of each directory: an environment variable name, or "default".
	Runtime string
	State   string
}

// OriginDefault is the origin string for a built-in default.
const OriginDefault = "default"

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
	d, _, err := DefaultWithOrigin()
	return d, err
}

// DefaultWithOrigin is Default, and also reports which rule chose each directory.
//
// Separate from Default so the common caller stays a two-value call, since only `cm config` needs the origin.
//
// Sockets stay under TMPDIR rather than moving to XDG_DATA_HOME, which was considered and rejected. The
// runtime directory holds nothing but sockets -- server.sock and one shim-NAME.sock per session, verified --
// and an abandoned socket in a temp directory is swept for free, where one in a persistent directory
// accumulates. cm copes with stale sockets either way, binding over them and reporting them through doctor,
// but self-cleaning is worth more than the 25 bytes of extra session-name budget a shorter path would buy.
// XDG_RUNTIME_DIR is honoured because that is freedesktop's directory for exactly this, and it is unset on
// macOS.
func DefaultWithOrigin() (Dirs, DirOrigin, error) {
	var (
		d      Dirs
		origin DirOrigin
	)

	if root := os.Getenv(Env("RUNTIME_DIR")); root != "" {
		d.Runtime = root
		origin.Runtime = "$" + Env("RUNTIME_DIR")
	} else if xdg := os.Getenv("XDG_RUNTIME_DIR"); xdg != "" {
		d.Runtime = filepath.Join(xdg, Name)
		origin.Runtime = "$XDG_RUNTIME_DIR"
	} else {
		tmp := os.TempDir()
		d.Runtime = filepath.Join(tmp, fmt.Sprintf("%s-%d", Name, os.Getuid()))
		origin.Runtime = OriginDefault
	}

	if root := os.Getenv(Env("STATE_DIR")); root != "" {
		d.State = root
		origin.State = "$" + Env("STATE_DIR")
	} else if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		d.State = filepath.Join(xdg, Name)
		origin.State = "$XDG_STATE_HOME"
	} else {
		home, err := os.UserHomeDir()
		if err != nil {
			return Dirs{}, DirOrigin{}, fmt.Errorf("resolving home directory: %w", err)
		}
		d.State = filepath.Join(home, ".local", "state", Name)
		origin.State = OriginDefault
	}

	return d, origin, nil
}

// Ensure creates the directories, owner-only. The sockets inside grant control of a
// shell, so the permissions are the access control.
func (d Dirs) Ensure() error {
	for _, dir := range []string{
		d.Runtime, d.State, d.logDir(),
		d.ServerLogDir(), d.ClientLogDir(), d.ShimLogDir(),
	} {
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

// Diagnostic logs live in a subdirectory per process type: logs/server, logs/client, logs/shim.
//
// Separated because there are three kinds of writer and the flat layout made them hard to tell apart by
// filename alone, which is what doctor's scanner had to do. It also keeps session *output* -- which sits
// directly in logs/ and is a different thing entirely -- from being mistaken for a diagnostic log.
const (
	serverLogSubdir = "server"
	clientLogSubdir = "client"
	shimLogSubdir   = "shim"
)

// ServerLogDir, ClientLogDir, and ShimLogDir are where each kind of diagnostic log lives.
//
// Exported so the scanner enumerates a directory rather than pattern-matching filenames, and so a caller
// adding a log has one place to look.
func (d Dirs) ServerLogDir() string { return filepath.Join(d.logDir(), serverLogSubdir) }
func (d Dirs) ClientLogDir() string { return filepath.Join(d.logDir(), clientLogSubdir) }
func (d Dirs) ShimLogDir() string   { return filepath.Join(d.logDir(), shimLogSubdir) }

// ServerLog is the server's diagnostic log.
func (d Dirs) ServerLog() string {
	return filepath.Join(d.ServerLogDir(), "server.log")
}

// ClientLog is the shared diagnostic log for every client.
//
// One file rather than one per client. A client is a short-lived process and there can be many -- one per
// attached window -- so a file each would accumulate without bound for diagnostics that are usually read
// only when something is wrong. Which client wrote a line is recorded as structured fields instead, so
// slog's output stays filterable: pid identifies the process, and boot identifies the boot it belongs to,
// since pids are reused and a log outlives a reboot.
func (d Dirs) ClientLog() string {
	return filepath.Join(d.ClientLogDir(), "client.log")
}

// ShimLog is one session's shim diagnostic log.
//
// Separate from the session's output log: this records what the shim did, while that records what the
// shell printed. Conflating them would make the output unreadable and the diagnostics unparseable.
//
// One file per session rather than shared, unlike clients: a shim lives as long as its session, there is
// exactly one per session, and its name is the obvious way to find it.
func (d Dirs) ShimLog(session string) string {
	return filepath.Join(d.ShimLogDir(), session+".log")
}

// SessionLog is the append-only output log for one session. Terminal output goes here
// rather than into sqlite: it is high-volume, written sequentially, and only ever read
// back in order.
//
// Directly in logs/ rather than a subdirectory, because it is not a diagnostic log: it is what the shell
// printed, and the subdirectories are for what cm's own processes recorded.
func (d Dirs) SessionLog(session string) string {
	return filepath.Join(d.logDir(), session+".log")
}
