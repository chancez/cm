package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/capability"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func newGrepCommand(g *globals) *cobra.Command {
	var (
		all         bool
		space       string
		tagArgs     []string
		grepCommand string
	)
	cmd := &cobra.Command{
		Use:   "grep <pattern> [session]",
		Short: "Search sessions' scrollback with grep or rg",
		Long: `Search the scrollback of one session, every session in a space, the sessions with a
tag, or every session, with an external grep.

Each session's contents are written as plain text, soft-wrapped lines rejoined, to a
private temporary directory as SPACE/NAME, and the command runs once over all of them
from inside it, so a match reads as "work/kitty.12:40: ...". The directory is removed
when the command exits.

The command is --grep-command, then CM_GREP_COMMAND, then [grep] command in the config
file, then rg if it is installed, then grep. It runs through sh -c and gets
"-e PATTERN --" and the files appended, so flags such as -i belong in the command:

  cm grep --grep-command 'rg -i' 'connection refused' --all

Exits with the command's status, so 1 means nothing matched. A session that cannot
be read is reported on stderr and makes the exit status at least 2, as grep does for
an unreadable file.`,
		Args:              cobra.RangeArgs(1, 2),
		ValidArgsFunction: completeSessionNames(g),
		RunE: func(cmd *cobra.Command, args []string) error {
			pattern := args[0]
			var named string
			if len(args) == 2 {
				named = args[1]
			}
			given := 0
			for _, set := range []bool{named != "", all, space != "", len(tagArgs) > 0} {
				if set {
					given++
				}
			}
			if given != 1 {
				return errors.New("give exactly one of a session, --space, --tag, or --all")
			}
			if err := validateSpaceArg(space); err != nil {
				return err
			}
			if err := validateSelectors(tagArgs); err != nil {
				return err
			}

			cfg, err := g.config()
			if err != nil {
				return err
			}
			command, err := resolveGrepCommand(grepCommand, cfg.Grep.Command, exec.LookPath)
			if err != nil {
				return err
			}
			dirs, err := g.dirs()
			if err != nil {
				return err
			}

			return withServer(cmd.Context(), g, func(ctx context.Context, cl serverv1.ServerClient) error {
				targets := []grepTarget{{ref: named, file: grepFileName("", named)}}
				if named == "" {
					if space != "" {
						if err := requireSpaces(ctx, cl); err != nil {
							return err
						}
					}
					resp, err := cl.List(ctx, &serverv1.ListRequest{Tags: tagArgs, Space: space})
					if err != nil {
						return err
					}
					if len(resp.Sessions) == 0 {
						return errors.New("no sessions to search")
					}
					targets = grepTargets(resp.Sessions)
				}

				// Private because scrollback is whatever passed through a terminal, secrets included. The
				// runtime directory is cm's own and 0700, and MkdirTemp makes this 0700 again inside it.
				dir, err := os.MkdirTemp(dirs.Runtime, "grep.")
				if err != nil {
					return err
				}
				defer os.RemoveAll(dir)

				paged := serverCapabilities(ctx, cl).Supports(capability.HistoryPage) == capability.Present
				var files []string
				failed := false
				for _, t := range targets {
					if err := writeHistoryFile(ctx, cl, paged, t, dir); err != nil {
						fmt.Fprintf(os.Stderr, "cm grep: %s: %v\n", t.ref, err)
						failed = true
						continue
					}
					files = append(files, t.file)
				}
				if len(files) == 0 {
					return &exitCodeError{code: 2, reported: true}
				}

				code, err := runGrep(ctx, command, pattern, dir, files, os.Stdout, os.Stderr)
				if err != nil {
					return err
				}
				if failed {
					code = max(code, 2)
				}
				if code != 0 {
					return &exitCodeError{code: code, reported: true}
				}
				return nil
			})
		},
	}
	f := cmd.Flags()
	f.BoolVar(&all, "all", false, "search every session")
	f.StringVar(&space, "space", "", "search the sessions in this space")
	f.StringArrayVar(&tagArgs, "tag", nil,
		"search the sessions with this tag, as key or key=value (repeatable, all must match)")
	f.StringVar(&grepCommand, "grep-command", "",
		"the search command, run through sh -c with -e PATTERN -- FILES appended (default: [grep] command, rg, then grep)")
	_ = cmd.RegisterFlagCompletionFunc("space", completeSpaceNames(g, nil))
	return cmd
}

// grepTarget is one session to search and the file its contents go in, relative to the search directory.
type grepTarget struct {
	ref  string
	file string
}

// grepTargets names each listed session's file SPACE/NAME, or NAME outside a space, so a match says where it
// came from in the words `cm list` uses, and orders them by that name.
func grepTargets(sessions []*serverv1.Session) []grepTarget {
	out := make([]grepTarget, 0, len(sessions))
	for _, s := range sessions {
		ref := s.GetName()
		if ref == "" {
			ref = "@" + s.GetId()
		}
		out = append(out, grepTarget{ref: "@" + s.GetId(), file: grepFileName(s.GetSpace(), ref)})
	}
	// By file, so the matches come out in an order that does not depend on the listing's.
	slices.SortFunc(out, func(a, b grepTarget) int { return strings.Compare(a.file, b.file) })
	return out
}

// grepFileName is where a session's contents go. A name cannot hold a slash, but a reference typed on the
// command line is not checked here, so one is replaced rather than trusted to stay inside the directory.
func grepFileName(space, ref string) string {
	clean := func(s string) string { return strings.ReplaceAll(s, string(filepath.Separator), "_") }
	if space == "" {
		return clean(ref)
	}
	return filepath.Join(clean(space), clean(ref))
}

// resolveGrepCommand picks the search command: the flag, which CM_GREP_COMMAND fills, then the config file,
// then rg, then grep.
//
// rg is preferred for speed, and the gap is large: 20 sessions of 100000 lines searched in 16ms with rg
// against 2.9s with the grep macOS ships, and 25ms against 6.6s with -i.
func resolveGrepCommand(flag, configured string, lookPath func(string) (string, error)) (string, error) {
	switch {
	case flag != "":
		return flag, nil
	case configured != "":
		return configured, nil
	}
	if _, err := lookPath("rg"); err == nil {
		return "rg -n", nil
	}
	if _, err := lookPath("grep"); err == nil {
		return "grep -n", nil
	}
	return "", errors.New("neither rg nor grep is on PATH; set [grep] command or --grep-command")
}

// historyClient is the part of the server's API writeHistoryFile reads with.
type historyClient interface {
	History(context.Context, *serverv1.HistoryRequest) (*serverv1.HistoryResponse, error)
	HistoryPage(context.Context, *serverv1.HistoryPageRequest) (*serverv1.HistoryPageResponse, error)
}

// writeHistoryFile writes a session's contents into its file under dir.
//
// A page at a time when the server can, which never holds the session's model for long and never meets the
// message size cap. A walk discarded under it, by pruning past the scrollback limit, a clear, or a reset,
// starts again once from the top: the contents changed shape, and what is there now is what a search should
// see. Against a server without pages it is one whole History.
func writeHistoryFile(ctx context.Context, cl historyClient, paged bool, t grepTarget, dir string) (err error) {
	path := filepath.Join(dir, t.file)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer func() {
		f.Close()
		// A session that could not be read leaves nothing behind, rather than part of one.
		if err != nil {
			os.Remove(path)
		}
	}()

	if !paged {
		resp, err := cl.History(ctx, &serverv1.HistoryRequest{Session: t.ref})
		if err != nil {
			return err
		}
		_, err = f.Write(resp.Data)
		return err
	}

	for attempt := 0; ; attempt++ {
		discarded, err := writePages(ctx, cl, t.ref, f)
		if err != nil || !discarded {
			return err
		}
		if attempt == 1 {
			return errors.New("its scrollback was discarded while being read, twice")
		}
		if err := f.Truncate(0); err != nil {
			return err
		}
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return err
		}
	}
}

// writePages copies one walk of a session's pages to w, reporting whether it was discarded part way.
func writePages(ctx context.Context, cl historyClient, ref string, w io.Writer) (discarded bool, err error) {
	cursor := ""
	for {
		resp, err := cl.HistoryPage(ctx, &serverv1.HistoryPageRequest{Session: ref, Cursor: cursor})
		if err != nil {
			return false, err
		}
		if resp.Discarded {
			return true, nil
		}
		if _, err := w.Write(resp.Data); err != nil {
			return false, err
		}
		if resp.Done {
			return false, nil
		}
		cursor = resp.Cursor
	}
}

// runGrep runs command over files from inside dir and returns its exit status.
//
// The pattern reaches the command as a positional parameter, "$@", never as part of the shell string, so no
// quoting in it can break out: the way git runs a pager or an alias. -e lets a pattern start with a dash, and
// -- keeps a file named like a flag from being read as one.
func runGrep(
	ctx context.Context, command, pattern, dir string, files []string, stdout, stderr io.Writer,
) (int, error) {
	args := append([]string{"-c", command + ` "$@"`, "sh", "-e", pattern, "--"}, files...)
	c := exec.CommandContext(ctx, "/bin/sh", args...)
	c.Dir = dir
	c.Stdin = nil
	c.Stdout = stdout
	c.Stderr = stderr
	err := c.Run()
	var exit *exec.ExitError
	if errors.As(err, &exit) {
		return exit.ExitCode(), nil
	}
	if err != nil {
		return 0, fmt.Errorf("running %q: %w", command, err)
	}
	return 0, nil
}
