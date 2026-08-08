package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newDoctorCommand(g *globals) *cobra.Command {
	var (
		clean  bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Report problems with this installation",
		Long: `Report problems with this installation, and optionally fix them.

Every check here corresponds to something that has actually gone wrong and took a
while to diagnose, because each fails silently rather than reporting an error.

  orphan-shim           a shim running with no session record, holding a pty and
                        a shell nothing can reattach to
  stale-socket          a socket file with nothing listening
  missing-shim          a session recorded as running whose shim is gone
  version-skew          client and server from different builds, where a feature
                        missing from one side waits forever instead of failing
  server-errors         errors in the server log, which nothing else surfaces
  no-terminal           a build without the emulator, so reattaching shows a
                        blank screen and 'cm history' is unavailable
  long-socket-path      a runtime directory close to the limit on a unix socket
                        path, which fails as an unexplained EINVAL
  no-shell-integration  sessions whose shells never report OSC 133, so cm cannot
                        tell busy from idle
  session-backlog       finished records piling up in 'cm list'
  pty-pressure          the system running low on ptys, which breaks terminal
                        allocation in unrelated programs
  loose-dir-perms       cm's directories readable by other users, exposing
                        session output and the sockets controlling live shells
  missing-log           a session record naming an output log that is gone, so
                        'cm history' and screen restore silently return nothing
  unreachable-shim      a session this server is tracking whose shim does not
                        answer, so it lists as running while commands fail
  unreachable-server    this server's own socket path no longer refers to it, so
                        clients silently start a second server while its sessions
                        stay stranded

--clean acts on orphan-shim, stale-socket, and loose-dir-perms. An orphan is
asked to shut down through its own socket rather than signalled, so it closes its
pty and reaps its shell; a shim that is not an orphan is never contacted.
Tightening a directory mode only takes access away from other users and leaves
cm's own behavior identical. Nothing else is repaired automatically: the rest are
either a record of something that already happened or a decision that is not a
diagnostic's to make.

Exits non-zero when anything is found, so it can gate a script.

Scoped to cm's own runtime directory: a shim whose runtime directory has been
deleted cannot be found this way, since there is nothing left to enumerate. That
case is worth knowing about, because it is silent -- commands keep working against
a replacement server while the original holds its sessions and their ptys. The
affected server reports it as unreachable-server, so run this against the same
directories the stranded server was started with.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Starts a server if none is running, rather than refusing.
			//
			// The first version declined, on the reasoning that a diagnostic should not change what it
			// examines. That was wrong in practice: the advice for an out-of-date server is to stop it, and
			// after doing so `cm doctor` failed with "no server is running", so following the instructions
			// produced a second error. Starting one also does not hide anything, since adoption is what a
			// healthy installation does and an orphan is by definition not adopted.
			conn, cl, err := connectServer(cmd.Context(), dirs)
			if err != nil {
				return err
			}
			defer conn.Close()

			return runDoctor(cmd.Context(), cl, clean, asJSON)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&clean, "clean", false, "fix what can be fixed")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// doctorJSON is the JSON shape of a diagnosis.
type doctorJSON struct {
	// Versions of both sides, so a report pasted into an issue says which builds it came from.
	ClientVersion string        `json:"client_version"`
	ServerVersion string        `json:"server_version"`
	Findings      []findingJSON `json:"findings"`
	// Repaired lists what --clean did, and is empty otherwise.
	Repaired []string `json:"repaired"`
}

// findingJSON is one problem.
type findingJSON struct {
	// Kind is a stable string, so a script can match on it as new kinds are added.
	Kind     string `json:"kind"`
	Session  string `json:"session"`
	Socket   string `json:"socket"`
	ShimPID  int32  `json:"shim_pid"`
	ShellPID int32  `json:"shell_pid"`
	Detail   string `json:"detail"`
	Fixable  bool   `json:"fixable"`
}

// runDoctor prints a diagnosis and reports failure as an exit status.
func runDoctor(ctx context.Context, cl serverv1.ServerClient, clean, asJSON bool) error {
	resp, err := cl.Doctor(ctx, &serverv1.DoctorRequest{
		Repair: clean,
		// Sent rather than derived server-side, because the server cannot tell which binary connected to it.
		ClientVersion: paths.Version(),
	})
	if err != nil {
		// A server too old to know this method cannot report anything, which is itself the diagnosis: it is
		// version skew, and the one case doctor cannot investigate from the inside. Said plainly, since
		// "Unimplemented desc = method Doctor" does not tell a reader to restart their server.
		if strings.Contains(err.Error(), "Unimplemented") {
			return fmt.Errorf(
				"the running server is too old to support this command (client is %s); "+
					"restart it with `cm server stop` and it will be replaced by this build",
				paths.Version())
		}
		return err
	}

	if asJSON {
		out := doctorJSON{
			ClientVersion: paths.Version(),
			ServerVersion: resp.ServerVersion,
			Findings:      make([]findingJSON, 0, len(resp.Findings)),
			Repaired:      resp.Repaired,
		}
		for _, f := range resp.Findings {
			out.Findings = append(out.Findings, findingJSON{
				Kind: f.Kind, Session: f.Session, Socket: f.Socket,
				ShimPID: f.ShimPid, ShellPID: f.ShellPid,
				Detail: f.Detail, Fixable: f.Fixable,
			})
		}
		if err := writeJSON(os.Stdout, out); err != nil {
			return err
		}
	} else {
		// Printed always, not only on a problem: the first question about any report is which builds
		// produced it, and a clean run is the one most likely to be pasted somewhere as evidence.
		fmt.Fprintf(os.Stdout, "client %s, server %s\n", paths.Version(), resp.ServerVersion)
		if len(resp.Findings) == 0 {
			fmt.Fprintln(os.Stdout, "no problems found")
		}
		for _, f := range resp.Findings {
			// The session is part of the heading only when there is one. Several checks describe the
			// installation rather than a session, and "kind: " with nothing after it reads like a bug.
			if f.Session != "" {
				fmt.Fprintf(os.Stdout, "%s: %s\n", f.Kind, f.Session)
			} else {
				fmt.Fprintf(os.Stdout, "%s\n", f.Kind)
			}
			fmt.Fprintf(os.Stdout, "  %s\n", f.Detail)
			if f.ShimPid != 0 {
				fmt.Fprintf(os.Stdout, "  shim pid %d, shell pid %d\n", f.ShimPid, f.ShellPid)
			}
		}
		for _, done := range resp.Repaired {
			fmt.Fprintf(os.Stdout, "fixed: %s\n", done)
		}
	}

	// Non-zero when a problem remains, so this can gate a script.
	//
	// Without --clean that is every finding. With it, a fixable finding that was repaired no longer counts,
	// while an unfixable one still does: it is still true, and reporting success with a real problem standing
	// would make the exit status worthless.
	remaining := len(resp.Findings) - len(resp.Repaired)
	if !clean {
		remaining = len(resp.Findings)
	}
	if remaining <= 0 {
		return nil
	}
	// Non-zero so this can gate a script, and reported already, so main does not print again.
	return &exitCodeError{code: 1, reported: true}
}
