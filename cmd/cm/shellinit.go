package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/osc"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/shellinit"
)

func newShellInitCommand() *cobra.Command {
	var noCompletions bool

	cmd := &cobra.Command{
		Use:   "shell-init <shell>",
		Short: "Print the shell integration to load in a session",
		Long: fmt.Sprintf(`Print the shell integration for a shell, to eval from its startup file.

  # zsh or bash
  eval "$(%[1]s shell-init zsh)"
  # fish
  %[1]s shell-init fish | source

This is the one line to add for a shell: it prints the completions from
'%[1]s completions' followed by the integration, so a startup file needs a single
%[1]s invocation rather than two. Measured: each one costs about 23ms.

Load it after compinit in zsh, or after whatever your shell uses to initialize
completion. The completion half asks the shell to register a completion function,
so it needs that machinery already in place; the integration half does not. If
that ordering is awkward, --no-completions prints the integration alone, which is
safe to load anywhere, and '%[1]s completions <shell>' can be loaded separately
wherever it fits.

Printed rather than installed. Editing your startup files is your business, and a
command that rewrites them has to guess which of several files a shell reads and
in what order, which is how a shell ends up with the integration loaded twice or
not at all.

The integration defines cm_report, a shell function that says what a session is doing:

  cm_report blocked "waiting for approval"
  cm_report busy    "running tests"
  cm_report idle
  cm_report clear

The state to reach for is blocked, because it is the one %[1]s cannot work out for
itself. Busy, idle, the last command and its exit status are already derived from
the OSC 133 markers your shell or prompt emits, so the integration does not
duplicate them, and loading it changes nothing about how those work.

Blocked is different in kind: a shell reports a command as running whether it is
computing or sitting at a prompt of its own, so only the program inside knows
which. Once reported it can be waited for:

  %[1]s wait reviewer --until blocked

The function writes a private escape sequence, OSC %[2]d, straight to the terminal.
That is why it exists alongside '%[1]s report', which does the same thing as a
command: a sequence costs nothing and works with no server running, while a
command costs about 23ms per call, which is too much for something a prompt hook
runs. Use '%[1]s report' from anything that is not a shell.

The integration does nothing outside a session, so only the ordering above
constrains where this goes.`,
			paths.Name, osc.ReportNumber),
		Args:      cobra.ExactArgs(1),
		ValidArgs: shellinit.Shells(),
		RunE: func(cmd *cobra.Command, args []string) error {
			shell := args[0]

			// The integration is read first, so an unsupported shell fails before anything is written.
			// Otherwise the completions would already be on stdout when the error came back, and a shell
			// eval'ing that output would load half a setup alongside the error.
			script, err := shellinit.Script(shell)
			if err != nil {
				return err
			}

			if !noCompletions {
				if err := writeCompletions(cmd.Root(), os.Stdout, shell); err != nil {
					return err
				}
			}

			_, err = os.Stdout.WriteString(script)
			return err
		},
	}

	cmd.Flags().BoolVar(&noCompletions, "no-completions", false,
		"print only the integration, without the completions")

	// Listed in the help as well as validated, since a caller has to know which shells exist before
	// guessing one.
	cmd.Short += " (" + strings.Join(shellinit.Shells(), ", ") + ")"
	return cmd
}
