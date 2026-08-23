package main

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/tags"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// errNoSessionGiven is returned when a command has neither a session name nor a selector.
var errNoSessionGiven = errors.New("no session given; name one or use --tag")

// resolveSelector turns a tag selector into the session names it matches.
//
// Client-side rather than passing the selector to each RPC, and that is a deliberate repeat of how
// `cm kill --all` already works: enumerating here keeps one meaning per request on the server, so a
// kill is always "kill these names" and a wait is always "wait for this session". A server that
// expanded a selector itself would have to grow the same expansion in every handler, and each would be
// a place where a selector matching nothing could silently mean "everything".
//
// The order is whatever `cm list` returns, which is oldest first with names breaking ties, so output
// across several sessions is stable between calls.
func resolveSelector(
	ctx context.Context, cl serverv1.ServerClient, selectors []string,
) ([]string, error) {
	resp, err := cl.List(ctx, &serverv1.ListRequest{Tags: selectors})
	if err != nil {
		return nil, err
	}
	sortSessions(resp.Sessions)
	names := make([]string, 0, len(resp.Sessions))
	for _, s := range resp.Sessions {
		names = append(names, s.Name)
	}
	return names, nil
}

// sessionTargets resolves the sessions a command should act on, from a name or a selector.
//
// Every command that accepts both wants the same three rules, so they live here rather than being
// restated: a selector and an explicit name together is a mistake rather than an intersection, since
// there is no reading of `cm read foo --tag bar` that is not a confusion; a selector matching nothing
// is an error rather than a silent success, because "no sessions matched" and "acted on all of them"
// must never be indistinguishable; and a bare name is not checked against the server here, so a
// command still reports its own not-found error.
//
// Returns fromSelector so a caller can tell one matched session from one named session. They print
// differently: a selector's output is headed even for a single match, since the caller did not know
// which session it would be.
func sessionTargets(
	ctx context.Context, cl serverv1.ServerClient, args, selectors []string,
) (names []string, fromSelector bool, err error) {
	named := len(args) > 0 && args[0] != ""

	switch {
	case named && len(selectors) > 0:
		return nil, false, fmt.Errorf(
			"session %q was named and --tag was given; use one or the other", args[0])
	case named:
		if err := paths.ValidateSessionRef(args[0]); err != nil {
			return nil, false, err
		}
		return []string{args[0]}, false, nil
	case len(selectors) == 0:
		return nil, false, errNoSessionGiven
	}

	names, err = resolveSelector(ctx, cl, selectors)
	if err != nil {
		return nil, false, err
	}
	if len(names) == 0 {
		// Named as a mismatch rather than reported as nothing to do. A script doing
		// `cm read --tag run=x` on a typo would otherwise read as a session with no output.
		return nil, false, fmt.Errorf("no sessions match %s", describeSelectors(selectors))
	}
	return names, true, nil
}

// describeSelectors renders a selector list for an error message.
func describeSelectors(selectors []string) string {
	if len(selectors) == 1 {
		return fmt.Sprintf("--tag %s", selectors[0])
	}
	out := ""
	for i, s := range selectors {
		if i > 0 {
			out += " "
		}
		out += "--tag " + s
	}
	return out
}

// validateSelectors checks a selector before a server is contacted.
//
// Worth doing client-side even though the server checks too: a typo should be reported the same way
// whether or not a server happens to be running, and starting one to learn that a flag was malformed is
// backwards.
func validateSelectors(selectors []string) error {
	_, err := tags.ParseSelector(selectors)
	return err
}

// sessionOrTagArg accepts at most one session name, since a selector supplies the rest.
//
// Separate from cobra.MaximumNArgs(1) so the error names --tag: a user who passed two names is usually
// reaching for the thing that takes several, and saying so is more use than "accepts at most 1 arg".
func sessionOrTagArg(cmd *cobra.Command, args []string) error {
	if len(args) > 1 {
		return fmt.Errorf(
			"expected at most one session name, got %d; use --tag to act on several", len(args))
	}
	return nil
}

// writeSessionHeader prints a delimiter naming which session the following output came from.
//
// Only when more than one session's output is being printed, or when a selector chose it: output from
// several sessions concatenated is unreadable without it, and a caller that named one session is
// piping that session's output and must not have a header spliced into it.
//
// The "=== name ===" shape matches what `skills/cm/SKILL.md` already tells an agent to write by hand
// around a fan-out's results, so the built-in form produces what the documented loop produced.
func writeSessionHeader(w io.Writer, name string, first bool) error {
	if !first {
		// Blank line between sessions rather than after each, so the last one does not trail one.
		if _, err := fmt.Fprintln(w); err != nil {
			return err
		}
	}
	_, err := fmt.Fprintf(w, "=== %s ===\n", name)
	return err
}
