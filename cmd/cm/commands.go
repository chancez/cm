package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"

	"github.com/chancez/cm/internal/paths"
)

// errNotImplemented marks a subcommand that is wired up but has no behavior yet, so the
// dispatch table can be exercised before the layers exist.
var errNotImplemented = errors.New("not implemented yet")

// newFlagSet builds a flag set that reports errors through run() rather than exiting,
// so a usage error still runs deferred cleanup such as restoring the terminal.
func newFlagSet(name string) *flag.FlagSet {
	fs := flag.NewFlagSet(paths.Name+" "+name, flag.ContinueOnError)
	fs.SetOutput(os.Stderr)
	return fs
}

func cmdAttach(ctx context.Context, args []string) error {
	fs := newFlagSet("attach")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("attach requires exactly one session name")
	}
	session := fs.Arg(0)
	if err := paths.ValidateSessionName(session); err != nil {
		return err
	}
	return fmt.Errorf("attach %s: %w", session, errNotImplemented)
}

func cmdList(ctx context.Context, args []string) error {
	fs := newFlagSet("list")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("list: %w", errNotImplemented)
}

func cmdKill(ctx context.Context, args []string) error {
	fs := newFlagSet("kill")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("kill requires exactly one session name")
	}
	session := fs.Arg(0)
	if err := paths.ValidateSessionName(session); err != nil {
		return err
	}
	return fmt.Errorf("kill %s: %w", session, errNotImplemented)
}

func cmdServer(ctx context.Context, args []string) error {
	fs := newFlagSet("server")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return fmt.Errorf("server: %w", errNotImplemented)
}

// cmdShim runs a session's shim. The server passes the session name and the pty size it
// wants; the shim owns the pty from then on.
func cmdShim(ctx context.Context, args []string) error {
	fs := newFlagSet(shimSubcommand)
	session := fs.String("session", "", "session name this shim serves")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if err := paths.ValidateSessionName(*session); err != nil {
		return err
	}
	return fmt.Errorf("shim %s: %w", *session, errNotImplemented)
}
