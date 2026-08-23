package main

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newTagCommand(g *globals) *cobra.Command {
	var (
		remove  []string
		replace bool
		asJSON  bool
	)
	cmd := &cobra.Command{
		Use:   "tag [session] [key=value]...",
		Short: "Label a session, so it can be grouped and filtered",
		Long: `Label a session with key/value tags, then filter on them with 'cm list --tag'.

  cm tag s7 project=cm role=reviewer
  cm tag s7 review                    # a key alone, with no value
  cm tag s7 --remove role
  cm ls --tag project=cm

With no session, the one this command is running in is used, from CM_SESSION, so
a program can tag itself.

Tags group sessions that a name cannot. A per-window session is named by the
server, so it is called "s17" and there is nothing for --prefix to match, and a
session belongs to several groupings at once while its name only says one thing.
A name also cannot change, whereas a tag can, so a session that turns out to be
something else can be relabelled.

Keys and values may contain letters, digits, '-', '_', '.', and '/', up to 63
bytes each. The restriction is not only tidiness: tags are printed by 'cm list',
and a value carrying an escape sequence could repaint or retitle the terminal of
whoever ran it.

By default the given tags are merged into the existing ones. --replace defines
the whole set instead, discarding any tag not named in this call.

cm never interprets a tag. There is no key that changes how a session is treated,
because inferring meaning from a tag would be the same mistake as scraping a
session's screen to work out what is running.`,
		Args: cobra.ArbitraryArgs,
		// Only the first argument can be a session name, and only when it is not a tag.
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			name, tagArgs, err := splitTagArgs(args)
			if err != nil {
				return err
			}
			if err := paths.ValidateSessionRef(name); err != nil {
				return err
			}
			set, err := tags.ParseAll(tagArgs)
			if err != nil {
				return err
			}
			for _, key := range remove {
				if err := tags.ValidateKey(key); err != nil {
					return err
				}
			}
			if len(set) == 0 && len(remove) == 0 && !replace {
				return errors.New("no tags given; pass key=value, --remove key, or --replace")
			}

			dirs, err := g.dirs()
			if err != nil {
				return err
			}
			// Deliberately not ensureServer, matching `cm report`: tagging a session implies one
			// exists, and starting a server here would create one that has never heard of it.
			return withServer(cmd.Context(), dirs, func(ctx context.Context, cl serverv1.ServerClient) error {
				resp, err := cl.Tag(ctx, &serverv1.TagRequest{
					Session: name,
					Set:     set,
					Remove:  remove,
					Replace: replace,
				})
				if err != nil {
					return err
				}
				if asJSON {
					// A map rather than a bare object, so a later field can be added without
					// changing the shape a script already reads.
					return writeJSON(os.Stdout, map[string]any{
						"session": name,
						"tags":    resp.Tags,
					})
				}
				// The resulting set, not just what changed: after a --remove or a --replace, what the
				// session now carries is the thing worth confirming.
				_, err = fmt.Println(tags.Format(resp.Tags))
				return err
			})
		},
	}
	f := cmd.Flags()
	f.StringArrayVar(&remove, "remove", nil, "remove this key (repeatable)")
	_ = cmd.RegisterFlagCompletionFunc("remove", completeTagKeys(g))
	f.BoolVar(&replace, "replace", false,
		"define the whole tag set, discarding tags not named here")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of text")
	return cmd
}

// splitTagArgs separates an optional leading session name from the tags that follow.
//
// Ambiguous by construction, since both are bare words: `cm tag build` could mean "tag the session I
// am in with the key build" or "tag the session named build with nothing". The '=' decides it, so a
// first argument containing one is a tag and anything else is the session name.
//
// That leaves `cm tag build` naming a session, which is the reading that fails loudly rather than
// quietly: tagging the wrong session with the right key is a mistake nothing reports, while naming a
// session and giving it no tags is refused by the caller. A program tagging itself has a session name
// available in CM_SESSION and can pass it, and `--remove` covers the rest.
func splitTagArgs(args []string) (session string, tagArgs []string, err error) {
	if len(args) > 0 && !isTagArg(args[0]) {
		session = args[0]
		return session, args[1:], nil
	}
	// Every argument is a tag, so the session is whichever one this is running in.
	session, err = sessionTarget(nil, "tag")
	if err != nil {
		return "", nil, err
	}
	return session, args, nil
}

// isTagArg reports whether an argument is a tag rather than a session name.
func isTagArg(arg string) bool {
	for _, r := range arg {
		if r == '=' {
			return true
		}
	}
	return false
}

// completeTagKeys offers the tag keys already in use, for --remove.
//
// Existing keys rather than nothing, because --remove only ever names a key the session already has,
// and those keys are chosen by whoever created the session so nothing else can guess them.
func completeTagKeys(g *globals) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		keys, err := tagKeys(cmd.Context(), g)
		if err != nil {
			// Silence rather than an error: a completion that prints a diagnostic corrupts the
			// command line being typed.
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		return keys, cobra.ShellCompDirectiveNoFileComp
	}
}

// tagKeys lists the distinct tag keys across all sessions.
func tagKeys(ctx context.Context, g *globals) ([]string, error) {
	dirs, err := g.dirs()
	if err != nil {
		return nil, err
	}
	conn, cl, err := dialServer(dirs)
	if err != nil {
		return nil, err
	}
	defer conn.Close()

	resp, err := cl.List(ctx, &serverv1.ListRequest{})
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	var out []string
	for _, s := range resp.Sessions {
		for k := range s.Tags {
			if _, dup := seen[k]; dup {
				continue
			}
			seen[k] = struct{}{}
			out = append(out, k)
		}
	}
	return out, nil
}
