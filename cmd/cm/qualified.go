package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"slices"
	"strings"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/remote"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Qualified references: `server:@a7k2m9x4` names a session on the remote called server, and `laptop:work`
// one here when this machine is laptop. The server never sees one. The client strips the host and points the
// command at it, exactly as --remote would, so every command that takes a session takes a qualified one, and
// a window can store one reference that means the same session from every machine. See docs/ideas.md,
// "Spaces, and sessions across hosts".

// routeBeforeArgs makes every command route qualified references before its arguments are validated.
//
// Before validation rather than in PersistentPreRunE, because cobra checks Args first, and validators such as
// sessionNameArg refuse the ':' a qualified reference carries. bindEnv is run here for the same ordering
// reason, since CM_REMOTE and CM_CONFIG decide what a host name means; it skips flags already set, so the
// PersistentPreRunE that runs it again changes nothing.
func routeBeforeArgs(g *globals, cmd *cobra.Command) {
	validate := cmd.Args
	cmd.Args = func(cmd *cobra.Command, args []string) error {
		if err := bindEnv(cmd); err != nil {
			return err
		}
		if err := g.resolveRemoteName(); err != nil {
			return err
		}
		if err := g.routeQualifiedRefs(cmd, args); err != nil {
			return err
		}
		if validate == nil {
			return cobra.ArbitraryArgs(cmd, args)
		}
		return validate(cmd, args)
	}
	for _, sub := range cmd.Commands() {
		routeBeforeArgs(g, sub)
	}
}

// sessionArgIndexes returns which of args are sessions, read off the command's usage line, the way
// completeSessionsMatching decides whether a command takes several.
//
// A word naming a session marks its own position, and only a word ending in "..." takes every argument from
// there on: `tag [session] [key=value]...` names one session, `kill <session>...` any number. Arguments after
// "--" are a command, never a session.
func sessionArgIndexes(cmd *cobra.Command, args []string) []int {
	words := strings.Fields(cmd.Use)
	if len(words) > 0 {
		words = words[1:]
	}
	limit := len(args)
	if dash := cmd.ArgsLenAtDash(); dash >= 0 && dash < limit {
		limit = dash
	}

	var out []int
	for i, w := range words {
		if w == "--" {
			break
		}
		if !strings.Contains(strings.ToLower(w), "session") {
			continue
		}
		if strings.HasSuffix(w, "...") {
			for j := i; j < limit; j++ {
				out = append(out, j)
			}
			break
		}
		if i < limit {
			out = append(out, i)
		}
	}
	return out
}

// resolveRemoteName clears --remote when it names this machine, so `--remote laptop` on laptop is the local
// server. Without it the same command line could not be used from every machine: it would ssh to itself.
//
// Clearing rather than resolving later, because a dozen places ask whether g.remote is empty to decide where
// a cwd, an environment or a calling session belongs.
func (g *globals) resolveRemoteName() error {
	if g.remote == "" || remote.ValidateName(g.remote) != nil {
		return nil
	}
	cfg, err := g.config()
	if err != nil {
		return err
	}
	local, err := cfg.LocalName()
	if err != nil {
		return err
	}
	if g.remote == local {
		g.remote = ""
	}
	return nil
}

// targetElsewhere marks a command whose session argument is a target on another host rather than where the
// command runs, so routing leaves it alone and the command reads the qualifier itself. `cm switch` and
// `cm rebind` move a window of this machine's: routed by the target's host, the request went to white's
// server, which was asked to move a window it has never seen and answered that the session had no clients.
const targetElsewhere = "cm.target-elsewhere"

// splitLocalTarget reads a switch target: the reference on this machine with any qualifier naming this
// machine stripped, and the host when it names another one.
func (g *globals) splitLocalTarget(target string) (ref, host string, err error) {
	h, rest, ok := remote.SplitRef(target)
	if !ok {
		return target, "", nil
	}
	cfg, err := g.config()
	if err != nil {
		return "", "", err
	}
	local, err := cfg.LocalName()
	if err != nil {
		return "", "", err
	}
	if h == local {
		return rest, "", nil
	}
	if _, ok, err := cfg.LookupRemote(h); err != nil {
		return "", "", err
	} else if !ok {
		// Left for the reference check to refuse, which names the host.
		return target, "", nil
	}
	return rest, h, nil
}

// qualifiedArg is one place a session reference was given: a positional argument or the --session flag.
type qualifiedArg struct {
	value string
	set   func(string) error
}

// routeQualifiedRefs strips the host from every qualified session reference cmd was given, and points the
// command at that host.
//
// Only a host this machine knows is routed: a configured remote or its own name. Anything else is left for
// the command's own validation to refuse, which matters because the optional session of `tag [session]
// [key=value]...` is the first argument's slot, and `cm tag url=http://x` must not read as host "url=http".
//
// One host per command. `cm kill server:a laptop:b` would need two connections and a result that says which
// half failed, and the scripts that want that can run two commands.
//
// The host is set as the --remote flag rather than only on globals, so it is seen everywhere --remote is,
// including the argv a client re-execs itself with on an upgrade: a window attached as `server:@id` would
// otherwise come back attached to the local session of that ID.
func (g *globals) routeQualifiedRefs(cmd *cobra.Command, args []string) error {
	if cmd.Annotations[targetElsewhere] != "" {
		return nil
	}
	var refs []qualifiedArg
	for _, i := range sessionArgIndexes(cmd, args) {
		refs = append(refs, qualifiedArg{value: args[i], set: func(v string) error { args[i] = v; return nil }})
	}
	// `cm shim --session` is the server naming a shim's session to itself, never typed by anyone.
	if f := cmd.Flags().Lookup("session"); f != nil && f.Changed && cmd.Name() != "shim" {
		refs = append(refs, qualifiedArg{value: f.Value.String(), set: func(v string) error {
			return cmd.Flags().Set("session", v)
		}})
	}
	// A name of this machine's that points to another host is followed, so a window restored by its name
	// lands where the name now points.
	if g.remote == "" && !namesActOnThemselves[commandKey(cmd)] {
		followed, err := g.followPointers(cmd.Context(), refs)
		if err != nil {
			return err
		}
		// A --dir given with a name of this machine's is a path here: a launch wrapper passes the window's
		// directory for a session it expects to be local. Followed to another host it names nothing there,
		// and a session recreated by the name would be asked to start in it, so the far end chooses instead.
		if f := cmd.Flags().Lookup("dir"); followed && f != nil && f.Changed {
			if err := f.Value.Set(""); err != nil {
				return err
			}
		}
	}

	qualified := refs[:0]
	for _, r := range refs {
		if strings.Contains(r.value, ":") {
			qualified = append(qualified, r)
		}
	}
	// Most commands, and every unqualified one, stop here without reading the config file.
	if len(qualified) == 0 {
		return nil
	}

	cfg, err := g.config()
	if err != nil {
		return err
	}
	local, err := cfg.LocalName()
	if err != nil {
		return err
	}
	remotes, err := cfg.Remotes()
	if err != nil {
		return err
	}
	known := map[string]bool{local: true}
	for _, r := range remotes {
		known[r.Name] = true
	}

	host := ""
	for _, r := range qualified {
		h, rest, _ := remote.SplitRef(r.value)
		if !known[h] {
			continue
		}
		if host != "" && h != host {
			return fmt.Errorf("sessions on %s and %s given together; name sessions on one host per command", host, h)
		}
		host = h
		if err := r.set(rest); err != nil {
			return err
		}
	}
	if host == "" {
		return nil
	}

	// A qualifier and --remote that disagree are refused rather than one winning, since either reading acts
	// on a session the other did not mean.
	if host == local {
		if g.remote != "" {
			return fmt.Errorf("%s is this machine, but --remote names %s", host, g.remote)
		}
		return nil
	}
	if g.remote != "" && g.remote != host {
		same, err := g.sameRemote(host)
		if err != nil {
			return err
		}
		if !same {
			return fmt.Errorf("a session on %s given, but --remote names %s", host, g.remote)
		}
	}
	return cmd.Flags().Set("remote", host)
}

// namesActOnThemselves are the commands for which a name is the thing acted on rather than a way to reach a
// session, so a pointer to another host is not followed: closing a window releases its name, wherever the
// name points, and renaming one renames it here. This machine's server handles a pointer given to these.
var namesActOnThemselves = map[string]bool{
	"kill":   true,
	"close":  true,
	"forget": true,
	"bind":   true,
	"unbind": true,
	"rename": true,
}

// followPointers rewrites each name that points to another host as the qualified reference it stands for.
//
// Read from this machine's database rather than asked of its server, so following one needs no server
// running, as attaching by name starts one only once it knows the name is local. A database that is not
// there, or is not this build's schema, holds no pointer this build can read, and the names are left as
// given: the server refuses a pointer it is handed rather than recreating it.
func (g *globals) followPointers(ctx context.Context, refs []qualifiedArg) (bool, error) {
	var names []int
	for i, r := range refs {
		if r.value != "" && !strings.ContainsAny(r.value, ":"+paths.IDSigil) {
			names = append(names, i)
		}
	}
	if len(names) == 0 {
		return false, nil
	}
	dirs, err := g.dirs()
	if err != nil {
		return false, nil
	}
	if _, err := os.Stat(dirs.Database()); err != nil {
		return false, nil
	}
	st, err := store.OpenExisting(ctx, dirs.Database())
	if err != nil {
		return false, nil
	}
	defer st.Close()
	followed := false
	for _, i := range names {
		rb, err := st.RemoteBindingFor(ctx, refs[i].value)
		if errors.Is(err, store.ErrNotFound) {
			continue
		}
		if err != nil {
			return false, err
		}
		if err := refs[i].set(rb.Qualified()); err != nil {
			return false, err
		}
		refs[i].value = rb.Qualified()
		followed = true
	}
	return followed, nil
}

// lookupOn finds the session ref names on the remote host.
func (g *globals) lookupOn(ctx context.Context, host, ref string) (*serverv1.Session, error) {
	there := *g
	there.remote = host
	var found *serverv1.Session
	err := withServer(ctx, &there, func(ctx context.Context, cl serverv1.ServerClient) error {
		resp, err := cl.List(ctx, &serverv1.ListRequest{})
		if err != nil {
			return err
		}
		for _, s := range resp.Sessions {
			if paths.FormatSessionID(s.Id) == ref || slices.Contains(s.Names, ref) {
				found = s
				return nil
			}
		}
		return fmt.Errorf("no session %s on %s", ref, host)
	})
	return found, err
}

// sameRemote reports whether --remote, given as an address, is the named remote host.
func (g *globals) sameRemote(host string) (bool, error) {
	cfg, err := g.config()
	if err != nil {
		return false, err
	}
	named, ok, err := cfg.LookupRemote(host)
	if err != nil || !ok {
		return false, err
	}
	given, err := g.remoteTarget()
	if err != nil {
		return false, err
	}
	return given != nil && *given == named.Target, nil
}

// completeRemoteNames offers the remotes in [remote.hosts] for --remote, each described by its address.
// An address typed in full is still accepted; this only offers the names, which are what a person types.
func completeRemoteNames(g *globals) func(*cobra.Command, []string, string) ([]string, cobra.ShellCompDirective) {
	return func(cmd *cobra.Command, args []string, toComplete string) ([]string, cobra.ShellCompDirective) {
		cfg, err := g.config()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		remotes, err := cfg.Remotes()
		if err != nil {
			return nil, cobra.ShellCompDirectiveNoFileComp
		}
		var out []string
		for _, r := range remotes {
			if strings.HasPrefix(r.Name, toComplete) {
				out = append(out, r.Name+"\t"+r.Target.String())
			}
		}
		return out, cobra.ShellCompDirectiveNoFileComp
	}
}

// sessionCompletions completes a session reference as typed so far, qualified or not.
//
// `white:<TAB>` asks white's server and offers its sessions as `white:name`, so what is offered is what
// routing will act on; this machine's own name asks this machine. Unqualified, the server this invocation
// names is asked as before, and the configured remotes are offered after its sessions as `white:`, with no
// space after when only hosts match, since a host alone is not a reference. A host a completion cannot
// reach offers nothing, within the bound every remote completion has.
func (g *globals) sessionCompletions(
	ctx context.Context, toComplete string, want func(*serverv1.Session) bool,
) ([]string, cobra.ShellCompDirective, error) {
	directive := cobra.ShellCompDirectiveNoFileComp
	var (
		local   string
		remotes []config.NamedRemote
	)
	// A config that does not load completes as it did before there were hosts, rather than not at all.
	if cfg, err := g.config(); err == nil {
		local, _ = cfg.LocalName()
		remotes, _ = cfg.Remotes()
	}

	if host, rest, ok := remote.SplitRef(toComplete); ok {
		isRemote := slices.ContainsFunc(remotes, func(r config.NamedRemote) bool { return r.Name == host })
		if host != local && !isRemote {
			return nil, directive, nil
		}
		// Pointed at the host for this one listing, as routing points the command, then put back.
		saved := g.remote
		g.remote = host
		if host == local {
			g.remote = ""
		}
		defer func() { g.remote = saved }()

		names, err := sessionNames(ctx, g, rest, want)
		if err != nil {
			return nil, directive, err
		}
		for i, n := range names {
			names[i] = host + ":" + n
		}
		return names, directive, nil
	}

	names, err := sessionNames(ctx, g, toComplete, want)
	var hosts []string
	for _, r := range remotes {
		if strings.HasPrefix(r.Name+":", toComplete) {
			hosts = append(hosts, r.Name+":\tsessions on "+r.Target.String())
		}
	}
	// The hosts are still worth offering when this machine's server is not running: they need no server.
	if err != nil && len(hosts) == 0 {
		return nil, directive, err
	}
	if len(names) == 0 && len(hosts) > 0 {
		directive |= cobra.ShellCompDirectiveNoSpace
	}
	return append(names, hosts...), directive, nil
}
