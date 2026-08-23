package main

import (
	"context"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/sessionenv"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newGetEnvCommand(g *globals) *cobra.Command {
	var (
		format string
		all    bool
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:   "get-env [session]",
		Short: "Print environment variables from the session's most recent client",
		Long: `Print the terminal-related environment variables the session's most recent
client reported.

A shell inside a session holds the values that existed when it started. Reattaching
from a different terminal, or the same one after it restarted, leaves those
describing a terminal that no longer exists. kitty's KITTY_LISTEN_ON is the usual
casualty: every kitten call goes through it and fails once that socket is gone.

Nothing outside a process can change its environment, so the shell has to apply
these itself. Add a hook to do it automatically:

  # zsh
  precmd() { eval "$(cm get-env --format=posix)" }

  # fish
  function cm_env --on-event fish_prompt
      cm get-env --format=fish | source
  end

With no session name, $` + paths.SessionEnv() + ` is used, so it works from inside a
session with no arguments.

By default only variables that differ from the current environment are printed,
and a name prefixed with '-' means the client no longer has it. Removals matter:
a stale socket path is worse than an absent one, because a client tries it and
fails rather than falling back.`,
		Args:              cobra.MaximumNArgs(1),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			session, err := resolveSession(args)
			if err != nil {
				return err
			}

			f, err := sessionenv.ParseFormat(format)
			if err != nil {
				return err
			}

			cfg, err := g.config()
			if err != nil {
				return err
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.GetEnv(ctx, &serverv1.GetEnvRequest{Session: session})
				if err != nil {
					return err
				}

				if asJSON {
					// The raw recorded values, not a diff: a script applying them has no shell
					// environment to diff against.
					env := resp.Env
					if env == nil {
						env = map[string]string{}
					}
					return writeJSON(os.Stdout, env)
				}

				d := sessionenv.Diff{Set: resp.Env}
				if !all {
					// Compare against this process's environment, which is the shell's, so only
					// actual changes are emitted. Without this a prompt hook would re-export
					// everything on every prompt.
					d = sessionenv.Compute(resp.Env, currentEnv(), cfg.EnvMatcher())
				}
				_, err = fmt.Fprint(os.Stdout, sessionenv.Render(d, f))
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&format, "format", "plain", "output format: plain, posix, or fish")
	f.BoolVar(&all, "all", false,
		"print every recorded variable, not only those that differ from the current environment")
	f.BoolVar(&asJSON, "json", false,
		"print every recorded variable as a JSON object, ignoring --format")
	return cmd
}

// resolveSession returns the session to act on, falling back to the one this process is running
// inside. That is what makes a prompt hook work with no arguments.
func resolveSession(args []string) (string, error) {
	if len(args) == 1 {
		if err := paths.ValidateSessionRef(args[0]); err != nil {
			return "", err
		}
		return args[0], nil
	}
	name := os.Getenv(paths.SessionEnv())
	if name == "" {
		return "", fmt.Errorf("no session given and %s is not set", paths.SessionEnv())
	}
	return name, nil
}

// currentEnv returns this process's environment as a map.
//
// Entries without an "=" are skipped rather than stored with an empty value. os.Environ should not produce any,
// but a process can be exec'd with arbitrary strings in its environment block, and a bare name is not a
// variable with an empty value: treating it as one would report a variable the caller never set.
func currentEnv() map[string]string {
	out := make(map[string]string)
	for _, kv := range os.Environ() {
		if k, v, ok := strings.Cut(kv, "="); ok {
			out[k] = v
		}
	}
	return out
}
