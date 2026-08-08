package main

import (
	"fmt"
	"os"
	"runtime"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/vt"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newVersionCommand(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "version",
		Short: "Print the version of this binary and of the running server",
		Long: `Print the version of this binary and of the running server.

Both, because in ` + paths.Name + ` they are routinely different: a session outlives the
server that created it, so after an upgrade a new binary talks to a server started
by the old one until it is restarted. A feature missing from one side fails
silently rather than reporting an error, so knowing both is the first step in
diagnosing anything.

Also prints whether this build has the terminal emulator, since without it
reattaching shows a blank screen and 'cm history' is unavailable, and that is
otherwise invisible.

The server is not started just to be asked. With none running the server line
says so, which is a normal state rather than a problem.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runVersion(cmd, g, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// versionJSON is the JSON shape of the version report.
type versionJSON struct {
	Client string `json:"client"`
	// Server is empty when none is running, which is distinguishable from an unknown version.
	Server string `json:"server,omitempty"`
	// ServerRunning separates "no server" from "a server that did not answer".
	ServerRunning bool   `json:"server_running"`
	Terminal      bool   `json:"terminal"`
	Go            string `json:"go"`
	Platform      string `json:"platform"`
}

func runVersion(cmd *cobra.Command, g *globals, asJSON bool) error {
	out := versionJSON{
		Client: paths.Version(),
		// Reported because a no-cgo build silently loses screen restore and history, and the symptom is a
		// blank screen on reattach, which looks like a bug in restore rather than a build without the
		// emulator.
		Terminal: vt.Available,
		Go:       runtime.Version(),
		Platform: runtime.GOOS + "/" + runtime.GOARCH,
	}

	// Asked only if a server is already there. Starting one to ask its version would make a diagnostic
	// change what it reports on, and "no server is running" is a useful answer in its own right.
	if dirs, err := g.dirs(); err == nil {
		conn, cl, derr := dialServer(dirs)
		if derr == nil {
			defer conn.Close()
			// Doctor carries the server's version and needs no session to exist. A dedicated RPC would be
			// tidier and this avoids adding one for a field that is already on the wire.
			resp, rerr := cl.Doctor(cmd.Context(), &serverv1.DoctorRequest{
				ClientVersion: paths.Version(),
			})
			switch {
			case rerr == nil:
				out.Server = resp.ServerVersion
				out.ServerRunning = true
			case isUnimplemented(rerr):
				// A server too old to answer. Saying so is the point: that is version skew, and it is the
				// one case this command cannot report a number for.
				out.Server = "too old to report"
				out.ServerRunning = true
			}
		}
	}

	if asJSON {
		return writeJSON(os.Stdout, out)
	}

	fmt.Fprintf(os.Stdout, "client   %s\n", out.Client)
	switch {
	case !out.ServerRunning:
		fmt.Fprintln(os.Stdout, "server   not running")
	default:
		fmt.Fprintf(os.Stdout, "server   %s\n", out.Server)
	}
	if out.ServerRunning && out.Server != out.Client && out.Server != "too old to report" {
		// Called out rather than left for the reader to compare, since the whole reason for printing both is
		// that a mismatch is silent. Not an error: cm is designed for a session to outlive its server.
		fmt.Fprintf(os.Stdout,
			"         (differs from this binary; restart with `%s server stop` to pick this one up)\n",
			paths.Name)
	}
	if !out.Terminal {
		fmt.Fprintln(os.Stdout, "terminal no (screen restore and history unavailable in this build)")
	} else {
		fmt.Fprintln(os.Stdout, "terminal yes")
	}
	fmt.Fprintf(os.Stdout, "go       %s\n", out.Go)
	fmt.Fprintf(os.Stdout, "platform %s\n", out.Platform)
	return nil
}

// isUnimplemented reports whether an error means the server does not know the method.
//
// Matched on the message because ttrpc does not export a sentinel for it. In one place so this command and
// doctor agree on what an old server looks like.
func isUnimplemented(err error) bool {
	return err != nil && strings.Contains(err.Error(), "Unimplemented")
}
