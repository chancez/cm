// Command cm is a terminal multiplexer that persists shell sessions.
//
// One binary provides all three layers. The client and server are user-facing
// subcommands; the shim is internal and launched by the server via re-exec, which is why
// it is hidden rather than documented. Go cannot fork(), so re-exec takes the place of
// the double-fork a C implementation would use to detach a session from its parent.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/chancez/cm/internal/paths"
)

func main() {
	// The server and shim are long-lived and must shut down in an orderly way so they
	// can unlink their sockets; a stale socket makes a session look alive when it is
	// not. Clients handle signals too, so they can restore the terminal on the way out.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := newRootCommand().ExecuteContext(ctx); err != nil {
		// Cobra has already printed usage errors and flag errors. Printing again would
		// duplicate them, so only report what it did not.
		if !errors.Is(err, errAlreadyReported) {
			fmt.Fprintf(os.Stderr, "%s: %v\n", paths.Name, err)
		}
		os.Exit(1)
	}
}

// errAlreadyReported marks an error whose message cobra has printed, so main does not
// print it twice.
var errAlreadyReported = errors.New("already reported")
