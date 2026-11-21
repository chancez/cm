package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newCloseCommand(g *globals) *cobra.Command {
	var (
		clientPID int32
		asJSON    bool
	)
	cmd := &cobra.Command{
		Use:   "close [SESSION]",
		Short: "Say a window showing a session was closed, and let the server decide what that means",
		Long: `Say a window showing a session was closed. For a terminal emulator's window-close
hook, in place of 'cm kill'.

Closing a window ends nothing that is running. The session is:

  kept       when another window still shows it; only this one's client detaches
  detached   when a command is running, a program reports work, or the shell has
             never reported a prompt, so nothing says it is idle
  shelved    when the shell is idle at a prompt and the session saves output,
             kept for the configured expire_after
  killed     when the shell is idle at a prompt and saves no output, which loses
             only its scrollback
  unbound    when SESSION is a name that borrowed it: the name is released

--client-pid names the closed window's client, so only it detaches. Without it
every client detaches. Run only on a close: a dropped connection is a detach and
nothing more.

With no session, the one this command runs in is used, from ` + paths.SessionEnv() + `.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			ref, err := resolveSession(args)
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireServerCapability(serverCapabilities(ctx, cl), capability.Close,
					"apply the window-close policy"); err != nil {
					return err
				}
				resp, err := cl.Close(ctx, &serverv1.CloseRequest{Session: ref, ClientPid: clientPID})
				if err != nil {
					return err
				}
				word := closeOutcomeWord(resp.Outcome)
				if asJSON {
					enc := json.NewEncoder(os.Stdout)
					enc.SetIndent("", "  ")
					return enc.Encode(struct {
						Session string `json:"session"`
						Outcome string `json:"outcome"`
					}{ref, word})
				}
				fmt.Printf("%s: %s\n", ref, word)
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.Int32Var(&clientPID, "client-pid", 0, "the closed window's client, so only it detaches")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// closeOutcomeWord is how an outcome is printed: the enum's own name, lower-cased, so the text and the JSON
// cannot drift from the proto.
func closeOutcomeWord(o serverv1.CloseOutcome) string {
	return strings.ToLower(strings.TrimPrefix(o.String(), "CLOSE_OUTCOME_"))
}
