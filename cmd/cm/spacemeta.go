package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/spaces"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// metaUnmetExit is the status for a key that is not as asked: absent for get, at another revision for a
// conditional set or remove. Distinct from 1, a failure, because a tool saving a layout has to tell "someone
// else saved since you read" apart from "the save did not happen", and the first wants a reload rather than
// a retry.
const metaUnmetExit = 2

func newSpaceMetaCommand(g *globals) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "meta",
		Short: "Read and write a space's metadata, values cm stores for other tools",
		Long: `Read and write a space's metadata: values stored on this server under a key,
which cm never reads. For a tool that keeps state beside the sessions, such as a
terminal's layout of a space's windows, so another machine viewing the space can
lay them out again.

  cm space meta set cm kspaces/layout/main --file layout.session
  cm space meta get cm kspaces/layout/main
  cm space meta list cm --prefix kspaces/

Keys are a space name's characters plus '/', up to 128 bytes, namespaced by
convention with the tool's name. Values are up to 64 KiB. Removing a space
removes its metadata.

Every write gives the key a new revision. --if-revision makes a write conditional
on the revision read, 0 meaning the key must not exist, so two machines saving
the same key cannot overwrite each other unseen. Exit status 2 means the key was
not as asked: absent for get, at another revision for --if-revision.`,
	}
	cmd.AddCommand(
		newSpaceMetaListCommand(g),
		newSpaceMetaGetCommand(g),
		newSpaceMetaSetCommand(g),
		newSpaceMetaRemoveCommand(g),
	)
	return cmd
}

// requireSpaceMeta refuses up front against a server without space metadata. See capability.SpaceMeta.
func requireSpaceMeta(ctx context.Context, cl serverv1.ServerClient) error {
	return requireServerCapability(serverCapabilities(ctx, cl), capability.SpaceMeta, "store space metadata")
}

// validateMetaArgs checks a space and key client-side, so a typo is reported whether or not a server runs.
func validateMetaArgs(space, key string) error {
	if err := spaces.ValidateName(space); err != nil {
		return err
	}
	return spaces.ValidateMetaKey(key)
}

// metaUnmet prints why a key was not as asked and exits with metaUnmetExit.
func metaUnmet(format string, args ...any) error {
	fmt.Fprintf(os.Stderr, "%s\n", fmt.Sprintf(format, args...))
	return &exitCodeError{code: metaUnmetExit, reported: true}
}

// conflictMessage says where a key is after a conditional write lost.
func conflictMessage(space, key string, current int64) string {
	if current == 0 {
		return fmt.Sprintf("%s in %s: does not exist", key, space)
	}
	return fmt.Sprintf("%s in %s: at revision %d", key, space, current)
}

// spaceMetaJSON is an entry as `cm space meta --json` prints it. Value is base64, as encoding/json writes
// bytes, since it is opaque and need not be text.
type spaceMetaJSON struct {
	Space     string    `json:"space"`
	Key       string    `json:"key"`
	Value     []byte    `json:"value,omitempty"`
	Size      uint32    `json:"size"`
	Revision  int64     `json:"revision"`
	UpdatedAt time.Time `json:"updated_at"`
}

func toSpaceMetaJSON(space string, e *serverv1.SpaceMetaEntry) spaceMetaJSON {
	return spaceMetaJSON{
		Space:     space,
		Key:       e.Key,
		Value:     e.Value,
		Size:      e.Size,
		Revision:  e.Revision,
		UpdatedAt: time.Unix(e.UpdatedAtUnix, 0),
	}
}

func encodeJSON(w io.Writer, v any) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func newSpaceMetaListCommand(g *globals) *cobra.Command {
	var (
		prefix string
		asJSON bool
	)
	cmd := &cobra.Command{
		Use:               "list SPACE",
		Aliases:           []string{"ls"},
		Short:             "List a space's metadata keys",
		Args:              cobra.ExactArgs(1),
		ValidArgsFunction: completeSpaceArg(g, nil),
		RunE: func(cmd *cobra.Command, args []string) error {
			space := args[0]
			if err := spaces.ValidateName(space); err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaceMeta(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.ListSpaceMeta(ctx, &serverv1.ListSpaceMetaRequest{Space: space, Prefix: prefix})
				if err != nil {
					return err
				}
				if asJSON {
					out := make([]spaceMetaJSON, 0, len(resp.Entries))
					for _, e := range resp.Entries {
						out = append(out, toSpaceMetaJSON(space, e))
					}
					return encodeJSON(os.Stdout, out)
				}
				return printSpaceMetaTable(os.Stdout, resp.Entries)
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&prefix, "prefix", "", "only keys starting with this")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of a table")
	return cmd
}

func printSpaceMetaTable(w io.Writer, entries []*serverv1.SpaceMetaEntry) error {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "KEY\tREVISION\tSIZE\tUPDATED")
	for _, e := range entries {
		fmt.Fprintf(tw, "%s\t%d\t%d\t%s\n", e.Key, e.Revision, e.Size,
			time.Unix(e.UpdatedAtUnix, 0).Format(time.DateTime))
	}
	return tw.Flush()
}

func newSpaceMetaGetCommand(g *globals) *cobra.Command {
	var asJSON bool
	cmd := &cobra.Command{
		Use:   "get SPACE KEY",
		Short: "Print a metadata value",
		Long: `Print a metadata value to stdout as stored, with nothing added. --json prints
it base64-encoded with its revision, which a conditional set needs.

Exit status 2 when the space has no value for KEY.`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMetaArgs(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			space, key := args[0], args[1]
			if err := validateMetaArgs(space, key); err != nil {
				return err
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaceMeta(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.GetSpaceMeta(ctx, &serverv1.GetSpaceMetaRequest{Space: space, Key: key})
				if err != nil {
					return err
				}
				if !resp.Found {
					return metaUnmet("%s in %s: does not exist", key, space)
				}
				if asJSON {
					return encodeJSON(os.Stdout, toSpaceMetaJSON(space, resp.Entry))
				}
				_, err = os.Stdout.Write(resp.Entry.Value)
				return err
			})
		},
	}
	cmd.Flags().BoolVar(&asJSON, "json", false, "print JSON, with the value base64-encoded")
	return cmd
}

// metaValue is the value a set writes: VALUE when given, else the contents of --file, '-' being stdin.
// Exactly one, so a set never reads a terminal waiting for input nobody knows to type.
func metaValue(args []string, file string, stdin io.Reader) ([]byte, error) {
	switch {
	case len(args) > 0 && file != "":
		return nil, errors.New("give VALUE or --file, not both")
	case len(args) > 0:
		return []byte(args[0]), nil
	case file == "-":
		return io.ReadAll(io.LimitReader(stdin, spaces.MaxMetaValueLen+1))
	case file != "":
		return os.ReadFile(file)
	}
	return nil, errors.New("give VALUE or --file")
}

func newSpaceMetaSetCommand(g *globals) *cobra.Command {
	var (
		file       string
		ifRevision int64
		asJSON     bool
	)
	cmd := &cobra.Command{
		Use:   "set SPACE KEY [VALUE]",
		Short: "Write a metadata value",
		Long: `Write a metadata value, VALUE or the contents of --file ('-' for stdin), and print
the key's new revision. The space is created if it does not exist.

--if-revision writes only if the key is at that revision, 0 meaning it must not
exist; otherwise nothing is written and the exit status is 2.`,
		Args:              cobra.RangeArgs(2, 3),
		ValidArgsFunction: completeMetaArgs(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			space, key := args[0], args[1]
			if err := validateMetaArgs(space, key); err != nil {
				return err
			}
			value, err := metaValue(args[2:], file, os.Stdin)
			if err != nil {
				return err
			}
			// Here as well as on the server, so a value too large fails before a connection is made.
			if len(value) > spaces.MaxMetaValueLen {
				return fmt.Errorf("value is over the %d-byte limit", spaces.MaxMetaValueLen)
			}
			req := &serverv1.SetSpaceMetaRequest{Space: space, Key: key, Value: value}
			if cmd.Flags().Changed("if-revision") {
				req.IfRevision = new(ifRevision)
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaceMeta(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.SetSpaceMeta(ctx, req)
				if err != nil {
					return err
				}
				if resp.Conflict {
					return metaUnmet("%s", conflictMessage(space, key, resp.CurrentRevision))
				}
				if asJSON {
					return encodeJSON(os.Stdout, struct {
						Space    string `json:"space"`
						Key      string `json:"key"`
						Revision int64  `json:"revision"`
					}{space, key, resp.Revision})
				}
				fmt.Println(resp.Revision)
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.StringVar(&file, "file", "", "read the value from this file, '-' for stdin")
	f.Int64Var(&ifRevision, "if-revision", 0, "write only if the key is at this revision, 0 for absent")
	f.BoolVar(&asJSON, "json", false, "print JSON instead of the revision")
	return cmd
}

func newSpaceMetaRemoveCommand(g *globals) *cobra.Command {
	var ifRevision int64
	cmd := &cobra.Command{
		Use:     "remove SPACE KEY",
		Aliases: []string{"rm"},
		Short:   "Remove a metadata key",
		Long: `Remove a metadata key. A key that does not exist is not an error, unless
--if-revision says it was expected at a revision, which exits 2 as set does.`,
		Args:              cobra.ExactArgs(2),
		ValidArgsFunction: completeMetaArgs(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			space, key := args[0], args[1]
			if err := validateMetaArgs(space, key); err != nil {
				return err
			}
			req := &serverv1.RemoveSpaceMetaRequest{Space: space, Key: key}
			if cmd.Flags().Changed("if-revision") {
				req.IfRevision = new(ifRevision)
			}
			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				if err := requireSpaceMeta(ctx, cl); err != nil {
					return err
				}
				resp, err := cl.RemoveSpaceMeta(ctx, req)
				if err != nil {
					return err
				}
				if resp.Conflict {
					return metaUnmet("%s", conflictMessage(space, key, resp.CurrentRevision))
				}
				return nil
			})
		},
	}
	cmd.Flags().Int64Var(&ifRevision, "if-revision", 0, "remove only if the key is at this revision")
	return cmd
}

// completeMetaArgs completes a space, then one of its keys, then nothing.
//
// Starts no server, for the reason completeSessionNames gives.
func completeMetaArgs(g *globals) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	spaceNames := completeSpaceNames(g, nil)
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		switch len(args) {
		case 0:
			return spaceNames(cmd, args, toComplete)
		case 1:
			keys, err := listMetaKeys(cmd.Context(), g, args[0], toComplete)
			if err != nil {
				// Silence, for the reason completeSpaceNames gives.
				return nil, cobra.ShellCompDirectiveNoFileComp
			}
			return keys, cobra.ShellCompDirectiveNoFileComp
		}
		return nil, cobra.ShellCompDirectiveNoFileComp
	}
}

// listMetaKeys asks whichever server this invocation names for a space's keys starting with prefix.
func listMetaKeys(ctx context.Context, g *globals, space, prefix string) ([]string, error) {
	ctx, cancel := g.completionDeadline(ctx)
	defer cancel()
	conn, cl, err := g.completionServer(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	resp, err := cl.ListSpaceMeta(ctx, &serverv1.ListSpaceMetaRequest{Space: space, Prefix: prefix})
	if err != nil {
		return nil, err
	}
	out := make([]string, 0, len(resp.Entries))
	for _, e := range resp.Entries {
		out = append(out, fmt.Sprintf("%s\trevision %d, %d bytes", e.Key, e.Revision, e.Size))
	}
	return out, nil
}
