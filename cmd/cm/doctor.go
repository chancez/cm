package main

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

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

  orphan-shim    a shim is running with no session record, holding a pty and a
                 shell that nothing can reattach to
  stale-socket   a socket file with nothing listening, left by a shim that died
  missing-shim   a session recorded as running whose shim is gone

--clean acts on the first two. An orphan is asked to shut down through its own
socket rather than signalled, so it closes its pty and reaps its shell; a shim
that is not an orphan is never contacted.

Exits non-zero when anything is found, so it can gate a script.

Worth running when terminals start failing to open. A shim holds a pty for as long
as it runs, and macOS allows 511 system-wide, so a leak of one per session is
invisible until something unrelated cannot allocate a terminal. Nothing in
'cm list' would show it, because an orphan is by definition not in the list.

Scoped to cm's own runtime directory: a shim whose runtime directory has been
deleted cannot be found this way, since there is nothing left to enumerate.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer. Diagnosing an installation must not change it, and starting a
			// server would adopt the very sessions being reported on.
			conn, cl, err := dialServer(dirs)
			if err != nil {
				return fmt.Errorf("no server is running, so there is nothing to diagnose: %w", err)
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
	Findings []findingJSON `json:"findings"`
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
	resp, err := cl.Doctor(ctx, &serverv1.DoctorRequest{Repair: clean})
	if err != nil {
		return err
	}

	if asJSON {
		out := doctorJSON{
			Findings: make([]findingJSON, 0, len(resp.Findings)),
			Repaired: resp.Repaired,
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
		if len(resp.Findings) == 0 {
			fmt.Fprintln(os.Stdout, "no problems found")
		}
		for _, f := range resp.Findings {
			fmt.Fprintf(os.Stdout, "%s: %s\n  %s\n", f.Kind, f.Session, f.Detail)
			if f.ShimPid != 0 {
				fmt.Fprintf(os.Stdout, "  shim pid %d, shell pid %d\n", f.ShimPid, f.ShellPid)
			}
		}
		for _, done := range resp.Repaired {
			fmt.Fprintf(os.Stdout, "fixed: %s\n", done)
		}
	}

	// Nothing left to report after a repair is success, since the problems are gone.
	remaining := len(resp.Findings) - len(resp.Repaired)
	if remaining <= 0 {
		return nil
	}
	// Non-zero so this can gate a script, and reported already, so main does not print again.
	return &exitCodeError{code: 1, reported: true}
}
