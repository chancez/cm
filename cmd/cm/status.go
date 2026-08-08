package main

import (
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newStatusCommand(g *globals) *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
		Short: "Print what the running server is doing",
		Long: `Print facts about the running server: its pid, how long it has been up,
the directories it is using, and how many sessions it holds.

Facts rather than problems, which is the difference from 'cm doctor'. Doctor
answers "is anything wrong"; this answers "what is running", and the two want
different amounts of work: doctor dials every shim and reads log files, where this
reads the session registry and one query.

The directories are worth seeing because a client and a server started with
different ones is a confusing state: commands appear to work while showing no
sessions. Comparing the pair here against 'cm config' settles it.

Does not start a server. With none running it says so, which is a normal state
rather than a problem.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runStatus(cmd, g, asJSON)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// statusJSON is the JSON shape of the status report.
type statusJSON struct {
	// Running separates "no server" from a server that answered.
	Running bool `json:"running"`
	// PID and the rest are meaningful only when Running.
	PID       int32  `json:"pid,omitempty"`
	Version   string `json:"version,omitempty"`
	UptimeSec int64  `json:"uptime_seconds,omitempty"`
	// StartedAt is absolute, so a report pasted somewhere stays meaningful after the fact.
	StartedAt  string `json:"started_at,omitempty"`
	RuntimeDir string `json:"runtime_dir,omitempty"`
	StateDir   string `json:"state_dir,omitempty"`
	Terminal   bool   `json:"terminal,omitempty"`

	SessionsRunning int32 `json:"sessions_running"`
	SessionsExited  int32 `json:"sessions_exited"`
	SessionsDead    int32 `json:"sessions_dead"`
	Clients         int32 `json:"clients"`

	// ClientVersion is this binary's build, so a mismatch is visible here too rather than only in
	// `cm version`.
	ClientVersion string `json:"client_version"`
}

func runStatus(cmd *cobra.Command, g *globals, asJSON bool) error {
	out := statusJSON{ClientVersion: paths.Version()}

	dirs, err := g.dirs()
	if err != nil {
		return err
	}

	// Asked only if a server is already there, like `cm version`: starting one so a report can describe it
	// would change what is being reported.
	conn, cl, derr := dialServer(dirs)
	if derr == nil {
		defer conn.Close()
		resp, rerr := cl.Status(cmd.Context(), &serverv1.StatusRequest{})
		switch {
		case rerr == nil:
			started := time.Unix(resp.StartedAtUnix, 0)
			out.Running = true
			out.PID = resp.Pid
			out.Version = resp.Version
			out.StartedAt = started.Format(time.RFC3339)
			out.UptimeSec = int64(time.Since(started).Seconds())
			out.RuntimeDir = resp.RuntimeDir
			out.StateDir = resp.StateDir
			out.Terminal = resp.Terminal
			out.SessionsRunning = resp.SessionsRunning
			out.SessionsExited = resp.SessionsExited
			out.SessionsDead = resp.SessionsDead
			out.Clients = resp.Clients

		case isUnimplemented(rerr):
			// A server predating this command. Reported as running, since it is, with the version left to
			// explain why nothing else is filled in.
			out.Running = true
			out.Version = "too old to report"
		default:
			return rerr
		}
	}

	if asJSON {
		return writeJSON(os.Stdout, out)
	}

	if !out.Running {
		fmt.Fprintln(os.Stdout, "server   not running")
		fmt.Fprintf(os.Stdout, "client   %s\n", out.ClientVersion)
		return nil
	}

	fmt.Fprintf(os.Stdout, "server   %s, pid %d\n", out.Version, out.PID)
	fmt.Fprintf(os.Stdout, "client   %s\n", out.ClientVersion)
	if out.Version != out.ClientVersion && out.Version != "too old to report" {
		// Flagged here as well as in `cm version`, because this is the command someone runs when something
		// looks wrong, and skew is the explanation often enough to be worth repeating.
		fmt.Fprintf(os.Stdout,
			"         (differs from this binary; `%s server restart` picks this one up)\n", paths.Name)
	}
	if out.UptimeSec > 0 {
		fmt.Fprintf(os.Stdout, "uptime   %s (since %s)\n",
			formatUptime(out.UptimeSec), out.StartedAt)
	}
	fmt.Fprintf(os.Stdout, "sessions %d running, %d exited, %d dead\n",
		out.SessionsRunning, out.SessionsExited, out.SessionsDead)
	fmt.Fprintf(os.Stdout, "clients  %d attached\n", out.Clients)
	if !out.Terminal {
		fmt.Fprintln(os.Stdout, "terminal no (this server cannot restore a screen)")
	}
	fmt.Fprintf(os.Stdout, "runtime  %s\n", out.RuntimeDir)
	fmt.Fprintf(os.Stdout, "state    %s\n", out.StateDir)
	return nil
}

// formatUptime renders a duration the way a person reads one.
//
// time.Duration.String gives "72h13m52.31s" for three days, which is accurate and hard to scan. The question
// this answers is usually "did the server restart recently", so the units that matter are the large ones.
func formatUptime(seconds int64) string {
	d := time.Duration(seconds) * time.Second
	days := int64(d.Hours()) / 24
	hours := int64(d.Hours()) % 24
	mins := int64(d.Minutes()) % 60

	switch {
	case days > 0:
		return fmt.Sprintf("%dd %dh", days, hours)
	case hours > 0:
		return fmt.Sprintf("%dh %dm", hours, mins)
	case mins > 0:
		return fmt.Sprintf("%dm", mins)
	default:
		return fmt.Sprintf("%ds", seconds)
	}
}
