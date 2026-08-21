package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/spf13/cobra"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// completionShells lists the shells cobra can generate completions for.
//
// A superset of the shells with an integration: powershell gets completions and has no cm_report,
// since the integration writes an OSC sequence from a POSIX-ish shell.
var completionShells = []string{"bash", "zsh", "fish", "powershell"}

func newCompletionsCommand() *cobra.Command {
	return &cobra.Command{
		Use:       "completions <shell>",
		Short:     "Print a shell completion script",
		Args:      cobra.ExactArgs(1),
		ValidArgs: completionShells,
		RunE: func(cmd *cobra.Command, args []string) error {
			return writeCompletions(cmd.Root(), os.Stdout, args[0])
		},
	}
}

// writeCompletions writes the completion script for a shell.
//
// Split out so `cm shell-init` can emit the same bytes rather than reimplementing the switch. The two
// commands drifting apart is the failure this avoids: a shell gaining completions here but not there
// would be invisible until someone noticed tab completion missing in one setup and not the other.
func writeCompletions(root *cobra.Command, w io.Writer, shell string) error {
	switch shell {
	case "bash":
		return root.GenBashCompletionV2(w, true)
	case "zsh":
		return root.GenZshCompletion(w)
	case "fish":
		return root.GenFishCompletion(w, true)
	case "powershell":
		return root.GenPowerShellCompletionWithDesc(w)
	default:
		return fmt.Errorf("unsupported shell %q, want one of %s",
			shell, strings.Join(completionShells, ", "))
	}
}

// completeSessionNames returns a completion function offering live session names.
//
// The names are what a user types most often and the least worth remembering, since implicit ones
// are allocated by the server and look like "s17".
//
// Deliberately does not start a server. Completion runs on every tab press, and a stray keystroke
// should not launch a daemon; if none is running there is nothing to complete anyway.
func completeSessionNames(g *globals) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		// Only the first positional argument is a session name for these commands.
		if len(args) > 0 && cmd.Name() != "kill" {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		names, err := sessionNames(cmd.Context(), g, toComplete)
		if err != nil {
			// Silence rather than an error: a completion that prints a diagnostic corrupts the
			// command line the user is typing.
			return nil, cobra.ShellCompDirectiveNoFileComp
		}

		// kill takes several, so names already given are dropped from the offers.
		if cmd.Name() == "kill" && len(args) > 0 {
			names = without(names, args)
		}
		return names, cobra.ShellCompDirectiveNoFileComp
	}
}

// sessionNames lists session names matching a prefix, described so the shell can show state.
func sessionNames(ctx context.Context, g *globals, prefix string) ([]string, error) {
	dirs, err := g.dirs()
	if err != nil {
		return nil, err
	}

	conn, cl, err := dialServer(dirs)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resp, err := cl.List(ctx, &serverv1.ListRequest{Prefix: prefix})
	if err != nil {
		return nil, err
	}

	sortSessions(resp.Sessions)
	out := make([]string, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		// The description after a tab is what zsh shows beside each candidate, which is where the
		// state is worth knowing: attaching to a dead session restores it, which is different from
		// attaching to a running one.
		desc := stateName(s)
		if s.Clients > 0 {
			desc += ", attached"
		}
		if s.Title != "" {
			desc += ", " + s.Title
		}
		out = append(out, s.Name+"\t"+desc)
	}
	return out, nil
}

// without returns names not present in exclude.
func without(names, exclude []string) []string {
	skip := make(map[string]struct{}, len(exclude))
	for _, e := range exclude {
		skip[e] = struct{}{}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		// The description is appended after a tab, so compare only the name.
		name, _, _ := strings.Cut(n, "\t")
		if _, drop := skip[name]; !drop {
			out = append(out, n)
		}
	}
	return out
}
