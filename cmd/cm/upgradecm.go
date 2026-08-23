package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// newUpgradeCommand moves the running parts of cm onto the installed build.
//
// Top level because it upgrades the whole installation, which is what earns the unqualified name. The rule
// the command tree follows is that a name says what it acts on: `cm clients upgrade` acts on clients, so it
// says so and could not have taken this name without promising more than it does.
//
// Installing a binary is still the package manager's job and this does not do it. What it does is the part
// no package manager can: make the processes already running adopt what was installed, which is otherwise
// invisible, since a server and its clients keep serving from the binary they started with.
//
// The two halves in order, which is the whole reason for one command. A client re-execs and reattaches, so
// upgrading clients before the server means each one comes back on the old server and then reconnects again
// when it restarts: two repaints per window instead of one, and a window briefly on a build the server does
// not match. Server first also means the version a client compares itself against is already the new one,
// so a client on the old build is asked and a client somehow already current is skipped.
//
// Between the two halves it waits for the clients to come back, and that wait is not defensive padding. A
// restart disconnects every client, each one reconnects on its own 100ms retry, and asking in the gap found
// nobody attached: the first version of this command reported "no clients were attached" with a window
// plainly attached, and the client then came back on the *old* binary. Only a real client showed it, because
// nothing that spawns no client can be absent from a listing.
func newUpgradeCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "upgrade",
		Short: "Move the running server and clients onto the installed build",
		Long: `Restart the server on the installed binary, then bring every client onto it.

This installs nothing. Install cm however you normally do, then run this to make what
is already running adopt it: a server keeps serving from the binary it started with,
and so does every attached client, so a fresh install changes nothing until something
replaces those processes.

  cm upgrade

The order matters and is why this is one command rather than two. A client re-execs
and reattaches, so upgrading clients first would bring each one back on the old
server and then make it reconnect when the server restarts: two repaints per window
instead of one. The server goes first, then the clients converge on it.

Nothing is lost either way. Sessions outlive the server by design, and a client holds
only a terminal and a stream position, both of which survive a re-exec, so windows
keep the screen they had.

Shims are the exception and are reported rather than replaced. A shim owns a pty and
the shell in it, so replacing one means ending that shell: a session that existed
before this command keeps the build it was started with until it ends. That is a
trade rather than a repair, so the count is printed and left to you. 'cm doctor'
reports how many builds the running shims span.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			ctx := cmd.Context()

			// Read before restarting, since the point of reporting a version is the change, and the old
			// one is only knowable from the server that is about to be replaced. A server that is not
			// running is not an error: this then starts one, which is an upgrade from nothing.
			before := serverStateBefore(ctx, dirs)

			if err := restartServer(ctx, dirs); err != nil {
				return err
			}

			out := upgradeReport{
				ServerBefore:  before.version,
				KeptShims:     before.sessions,
				ClientsBefore: before.clients,
				Asked:         map[string]uint32{},
				AlreadyOn:     map[string]uint32{},
			}

			err = withServer(ctx, dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				status, serr := cl.Status(ctx, &serverv1.StatusRequest{})
				if serr != nil {
					return serr
				}
				out.ServerAfter = status.Version

				// Wait for the clients the old server had to reconnect, or there is nothing to ask.
				out.ClientsReturned = waitForClients(ctx, cl, before.clients)

				resp, uerr := cl.UpgradeClients(ctx, &serverv1.UpgradeClientsRequest{})
				if uerr != nil {
					return uerr
				}
				for name, n := range resp.Asked {
					out.Asked[name] = n
				}
				for name, n := range resp.AlreadyCurrent {
					out.AlreadyOn[name] = n
				}
				out.Errors = resp.Errors
				return nil
			})
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSON(os.Stdout, out)
			}
			return printUpgradeReport(os.Stdout, out)
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// upgradeReport is what an upgrade did, and is the JSON shape.
type upgradeReport struct {
	// ServerBefore is empty when nothing was running, which is how "started one" is distinguished from
	// "replaced one".
	ServerBefore string `json:"server_before"`
	ServerAfter  string `json:"server_after"`
	// Asked and AlreadyOn count clients per session: asked to come back, and left alone for already
	// running the server's build.
	Asked     map[string]uint32 `json:"asked"`
	AlreadyOn map[string]uint32 `json:"already_current"`
	// KeptShims is how many sessions existed before the restart, and therefore still hold the shim that
	// spawned them. Reported because it is the part of an installation an upgrade cannot reach.
	KeptShims int `json:"kept_shims"`
	// ClientsBefore and ClientsReturned bracket the restart: how many clients the old server had, and how
	// many had reconnected by the time this asked them to upgrade.
	//
	// Reported because a shortfall means some window was not asked and is still on the old build, which is
	// the one failure of this command that would otherwise look like success.
	ClientsBefore   int               `json:"clients_before"`
	ClientsReturned int               `json:"clients_returned"`
	Errors          map[string]string `json:"errors,omitempty"`
}

// clientReturnTimeout bounds the wait for clients to reconnect after the restart.
//
// Generous against what it is waiting for: a client retries every 100ms for its first three seconds of an
// outage, so the ones that are coming back are back almost immediately. The length is for the case where one
// is not coming back at all, where the only alternatives are waiting this long once or asking nobody.
const clientReturnTimeout = 10 * time.Second

// clientReturnInterval is how often the wait re-counts. Matched to the client's own retry, since counting
// faster than they can arrive only spends requests.
const clientReturnInterval = 100 * time.Millisecond

// waitForClients waits for want clients to be attached, returning how many were by the end.
//
// Returns rather than errors on a shortfall: the upgrade should still ask whoever did come back, and the
// caller reports the difference. A client that never returns is a client that is gone, which is not a
// failure of the upgrade.
func waitForClients(ctx context.Context, cl serverv1.ServerClient, want int) int {
	if want <= 0 {
		return 0
	}
	deadline := time.Now().Add(clientReturnTimeout)
	for {
		got := 0
		if listed, err := cl.List(ctx, &serverv1.ListRequest{}); err == nil {
			for _, s := range listed.Sessions {
				got += int(s.Clients)
			}
		}
		if got >= want || time.Now().After(deadline) {
			return got
		}
		select {
		case <-ctx.Done():
			return got
		case <-time.After(clientReturnInterval):
		}
	}
}

// serverStateBefore reads what is running, tolerating there being nothing.
//
// Every failure is treated as "no server": this runs immediately before replacing whatever is there, so a
// server that cannot be reached or cannot answer is one whose version simply goes unreported, and refusing
// to upgrade over it would be refusing exactly when an upgrade is most wanted.
func serverStateBefore(ctx context.Context, dirs paths.Dirs) (state struct {
	version  string
	sessions int
	clients  int
}) {
	conn, cl, err := dialServer(dirs)
	if err != nil {
		return state
	}
	defer conn.Close()

	if status, serr := cl.Status(ctx, &serverv1.StatusRequest{}); serr == nil {
		state.version = status.Version
	}
	// Counted from the listing rather than from the status counts, because what matters here is sessions
	// whose shims predate the restart, which is every session that exists right now regardless of state.
	if listed, lerr := cl.List(ctx, &serverv1.ListRequest{}); lerr == nil {
		state.sessions = len(listed.Sessions)
		for _, s := range listed.Sessions {
			// Every attached client, read-only followers included: they reconnect like any other and can
			// re-exec like any other.
			state.clients += int(s.Clients)
		}
	}
	return state
}

// printUpgradeReport writes what changed, and says nothing else.
//
// One line, because on a healthy run there is one fact: which build everything is on now. What was left out
// is deliberate rather than terse for its own sake.
//
// Shims are not mentioned. A session that predates the restart keeps its shim, which is true on every run
// where any session exists, so printing it made the most repeated line the least actionable one; `cm doctor`
// reports how many builds the running shims span, which is the form worth reading. The count stays in
// --json for anything that wants it.
//
// The verb covers the server only. A client is *asked* to come back and the server cannot observe whether it
// did, which the RPC's own documentation is careful about, so the clients appear as a count rather than as
// something claimed to have happened.
//
// Anything that did not converge goes to stderr as a warning, the way a kill reports processes that survived
// it: loud enough to notice, and off the stream a script is reading.
func printUpgradeReport(w io.Writer, out upgradeReport) error {
	var line string
	switch {
	case out.ServerBefore == "":
		line = fmt.Sprintf("started on %s", out.ServerAfter)
	case out.ServerBefore == out.ServerAfter:
		// Not called an upgrade, since running this without installing anything is a reasonable thing to
		// do and reporting "x -> x" as a change would be a lie.
		line = fmt.Sprintf("restarted on %s", out.ServerAfter)
	default:
		line = fmt.Sprintf("upgraded to %s", out.ServerAfter)
	}
	// Clients that are on this build now: the ones asked, plus the ones already there. Zero prints
	// nothing rather than "no clients", which is the ordinary state of a server nobody is watching.
	if onBuild := total(out.Asked) + total(out.AlreadyOn); onBuild > 0 {
		line += ", " + plural(onBuild, "client")
	}
	if _, err := fmt.Fprintln(w, line); err != nil {
		return err
	}

	if missing := out.ClientsBefore - out.ClientsReturned; missing > 0 {
		// Named with the build they are left on when it is known, since that is what makes the warning
		// actionable: those windows are the ones to close or upgrade by hand.
		keeps := ""
		if out.ServerBefore != "" {
			keeps = fmt.Sprintf(" and keep %s", out.ServerBefore)
		}
		fmt.Fprintf(os.Stderr, "%s: warning: %s did not reconnect in %s%s\n",
			paths.Name, plural(missing, "client"), clientReturnTimeout, keeps)
	}

	if len(out.Errors) == 0 {
		return nil
	}
	for _, name := range sortedKeys(out.Errors) {
		fmt.Fprintf(os.Stderr, "%s: %s: %s\n", paths.Name, name, out.Errors[name])
	}
	return errAlreadyReported
}

// total sums a per-session count.
func total(counts map[string]uint32) int {
	sum := 0
	for _, n := range counts {
		sum += int(n)
	}
	return sum
}
