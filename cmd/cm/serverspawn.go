package main

import (
	"encoding/gob"
	"fmt"
	"os"
	"syscall"

	"github.com/spf13/cobra"
	"golang.org/x/sys/unix"
)

// newServerSpawnCommand builds `cm server spawn`, which starts a detached server and exits.
//
// It exists to be the first process of a launchd job on macOS, so that the server it starts is in launchd's
// coalition for that job rather than in the coalition of whatever app started the client: see startServer in
// serverstart_darwin.go. Exiting at once is the point, since the job is removed as soon as the server answers,
// and the server outlives it because it is in a session of its own.
//
// What the server would have inherited from the client arrives in a file, the same way a client hands the
// server the environment for a shell it asks for: a launchd job otherwise starts with launchd's, and every
// process under the server inherits whatever the server had. See spawnRequest for what it holds and why each
// part is there.
//
// The path is an argument rather than a flag because every flag has a CM_<FLAG> variable, and a stale export
// of one would point a server at a file nobody wrote.
//
// Hidden because nobody types it.
func newServerSpawnCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "spawn REQUEST-FILE",
		Short: "Start a detached server as REQUEST-FILE describes, and exit",
		Long: `Start a detached server as REQUEST-FILE describes, and exit.

What a launchd job runs on macOS to start a server outside the coalition of the
app that started the client. REQUEST-FILE holds the client's environment and
resource limits, and is removed once read.`,
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			req, err := readSpawnRequest(args[0])
			if err != nil {
				return err
			}
			exe, err := os.Executable()
			if err != nil {
				return fmt.Errorf("resolving own path to start a server: %w", err)
			}
			req.applyLimits()
			return spawnServer(exe, dirs, req.Env)
		},
	}
}

// spawnRequest is what a client hands `cm server spawn` so the server starts as if it were the client's child.
//
// Env is the client's environment exactly. It is in this file rather than in the launchd plist because a plist
// cannot carry every value an environment can hold: XML 1.0 has no spelling for most control characters, and
// an exported escape sequence is ordinary, in LESS_TERMCAP_* for one. gob rather than JSON for the same reason,
// since JSON replaces invalid UTF-8 and an environment value is bytes.
//
// Limits are the client's resource limits. Without them a session's `ulimit -n` read 256, launchd's soft
// default for maxfiles, against 1048576 when the same client started the server directly, and a build or a
// language server fails at 256 with "too many open files" and nothing about why.
//
// One difference from a direct child remains. Go raises a process's soft open-files limit to the most it may
// have at startup and restores the original only in children it starts, without exposing the original. So
// the client can report only the raised value, and sessions start with that: 138240 here, where a direct child
// got the 1048576 the client's own parent had. The kernel caps a process at kern.maxfilesperproc either way.
type spawnRequest struct {
	Env    []string
	Limits []spawnLimit
}

// spawnLimit is one resource limit.
type spawnLimit struct {
	Resource int
	Cur      uint64
	Max      uint64
}

// inheritedLimits are the resource limits a child inherits from its parent.
//
// RLIMIT_RSS is left out because on darwin it is RLIMIT_AS under another name.
var inheritedLimits = []int{
	unix.RLIMIT_AS,
	unix.RLIMIT_CORE,
	unix.RLIMIT_CPU,
	unix.RLIMIT_DATA,
	unix.RLIMIT_FSIZE,
	unix.RLIMIT_MEMLOCK,
	unix.RLIMIT_NOFILE,
	unix.RLIMIT_NPROC,
	unix.RLIMIT_STACK,
}

// newSpawnRequest describes this process, which is what the server would otherwise have inherited.
func newSpawnRequest() spawnRequest {
	req := spawnRequest{Env: os.Environ()}
	for _, res := range inheritedLimits {
		var lim syscall.Rlimit
		if err := syscall.Getrlimit(res, &lim); err == nil {
			req.Limits = append(req.Limits, spawnLimit{Resource: res, Cur: lim.Cur, Max: lim.Max})
		}
	}
	return req
}

// applyLimits sets this process's limits to the client's, so the server started next inherits them.
//
// Setting the open-files limit is also what stops Go restoring its own original, which here is launchd's, in
// every child it starts.
//
// A hard limit above this process's own cannot be raised to without privilege. A limit refused as given is
// retried clamped to the current hard limit, and one refused that way too is left as launchd set it: a server
// with a lower limit than its client is still a server, and refusing to start one would take every window with
// it.
func (r spawnRequest) applyLimits() {
	for _, l := range r.Limits {
		want := syscall.Rlimit{Cur: l.Cur, Max: l.Max}
		if syscall.Setrlimit(l.Resource, &want) == nil {
			continue
		}
		var cur syscall.Rlimit
		if syscall.Getrlimit(l.Resource, &cur) != nil {
			continue
		}
		clamped := syscall.Rlimit{Cur: min(l.Cur, cur.Max), Max: min(l.Max, cur.Max)}
		_ = syscall.Setrlimit(l.Resource, &clamped)
	}
}

// writeSpawnRequest records req for `cm server spawn`, readable only by this user since it holds whatever
// credentials the client's environment does.
func writeSpawnRequest(path string, req spawnRequest) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if err := gob.NewEncoder(f).Encode(req); err != nil {
		f.Close()
		return fmt.Errorf("writing the server's start request: %w", err)
	}
	return f.Close()
}

// readSpawnRequest reads what writeSpawnRequest wrote and removes the file, so the environment is on disk only
// between the client starting the job and the job starting the server.
func readSpawnRequest(path string) (spawnRequest, error) {
	f, err := os.Open(path)
	if err != nil {
		return spawnRequest{}, fmt.Errorf("reading the server's start request: %w", err)
	}
	defer f.Close()
	_ = os.Remove(path)

	var req spawnRequest
	if err := gob.NewDecoder(f).Decode(&req); err != nil {
		return spawnRequest{}, fmt.Errorf("reading the server's start request: %w", err)
	}
	// Never nil, which exec reads as "inherit this process's", and this process's is launchd's.
	if req.Env == nil {
		req.Env = []string{}
	}
	return req, nil
}
