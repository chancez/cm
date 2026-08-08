// Command cm is a terminal multiplexer that persists shell sessions.
//
// One binary provides all three layers. The client and server are user-facing
// subcommands; the shim is internal and launched by the server via re-exec, which is
// why it is hidden rather than documented. Go cannot fork(), so re-exec takes the place
// of the double-fork a C implementation would use to detach a session from its parent.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chancez/cm/internal/paths"
)

// shimSubcommand is not listed in usage: it is an implementation detail of how the
// server starts a session, and running it by hand does nothing useful.
const shimSubcommand = "shim"

func main() {
	if err := run(os.Args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			os.Exit(2)
		}
		fmt.Fprintf(os.Stderr, "%s: %v\n", paths.Name, err)
		os.Exit(1)
	}
}

func run(args []string) error {
	if len(args) == 0 {
		usage(os.Stderr)
		return errors.New("no subcommand given")
	}

	// The server and shim are long-lived and must shut down in an orderly way so they
	// can unlink their sockets; a stale socket makes a session look alive when it is
	// not. Clients cancel on signal too, so they can restore the terminal on the way
	// out.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	name, rest := args[0], args[1:]
	switch name {
	case "attach":
		return cmdAttach(ctx, rest)
	case "list", "ls":
		return cmdList(ctx, rest)
	case "kill":
		return cmdKill(ctx, rest)
	case "server":
		return cmdServer(ctx, rest)
	case shimSubcommand:
		return cmdShim(ctx, rest)
	case "help", "-h", "--help":
		usage(os.Stdout)
		return nil
	default:
		usage(os.Stderr)
		return fmt.Errorf("unknown subcommand %q", name)
	}
}

func usage(w *os.File) {
	fmt.Fprintf(w, `%[1]s manages persistent terminal sessions.

Usage:
  %[1]s attach <session>   attach to a session, creating it if needed
  %[1]s list               list sessions
  %[1]s kill <session>     terminate a session and its shell
  %[1]s server             run the server in the foreground

Run '%[1]s <subcommand> -h' for subcommand flags.
`, paths.Name)
}
