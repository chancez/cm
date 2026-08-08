package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
)

// errNotImplemented marks a subcommand that is wired up but has no behavior yet.
var errNotImplemented = errors.New("not implemented yet")

func newAttachCommand(g *globals) *cobra.Command {
	var (
		own      bool
		readOnly bool
	)
	cmd := &cobra.Command{
		Use:   "attach <session>",
		Short: "Attach to a session, creating it if needed",
		Long: `Attach to a session, creating it if it does not exist.

Being idempotent is what lets a terminal emulator use one command for both
creating a window's session and reattaching to it after a restart.`,
		Args: sessionNameArg,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("attach %s: %w", args[0], errNotImplemented)
		},
	}
	f := cmd.Flags()
	f.BoolVar(&own, "own", false,
		"terminate the session if this client disconnects without detaching")
	f.BoolVar(&readOnly, "read-only", false,
		"follow the session without sending input")
	return cmd
}

func newListCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:     "list",
		Aliases: []string{"ls"},
		Short:   "List sessions",
		Args:    cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("list: %w", errNotImplemented)
		},
	}
}

func newKillCommand(g *globals) *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "kill <session>...",
		Short: "Terminate sessions and their shells",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			for _, name := range args {
				if err := paths.ValidateSessionName(name); err != nil {
					return err
				}
			}
			return fmt.Errorf("kill: %w", errNotImplemented)
		},
	}
	cmd.Flags().BoolVar(&force, "force", false,
		"forget the session even if its shim cannot be reached")
	return cmd
}

func newServerCommand(g *globals) *cobra.Command {
	return &cobra.Command{
		Use:   "server",
		Short: "Run the server in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return fmt.Errorf("server: %w", errNotImplemented)
		},
	}
}

func newCompletionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completions <shell>",
		Short:     "Print a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: []string{"bash", "zsh", "fish", "powershell"},
		RunE: func(cmd *cobra.Command, args []string) error {
			root := cmd.Root()
			switch args[0] {
			case "bash":
				return root.GenBashCompletionV2(os.Stdout, true)
			case "zsh":
				return root.GenZshCompletion(os.Stdout)
			case "fish":
				return root.GenFishCompletion(os.Stdout, true)
			case "powershell":
				return root.GenPowerShellCompletionWithDesc(os.Stdout)
			default:
				return fmt.Errorf("unsupported shell %q", args[0])
			}
		},
	}
}
