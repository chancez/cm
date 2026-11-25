package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"google.golang.org/protobuf/encoding/protojson"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/store"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// Listing every host at once: `cm ls --all-hosts`. A client-side fan-out over the remotes in [remote.hosts],
// never a server re-serving them, which is decided under `--remote` in docs/ideas.md. Each session comes back
// with the host it is on, so everything that renders a session renders a merged list too.

// hostListTimeout bounds asking one remote, the bound a completion has, for its reason: a host that is
// unreachable rather than slow is shown from its last listing instead of holding the command.
const hostListTimeout = completionTimeout

// listingCacheDir is where each remote's last good listing is kept, under the state directory so it outlives
// a reboot: a laptop asked about a server it cannot reach still says what was running there, and when.
const listingCacheDir = "remote-listings"

// hostListing is one host's part of a merged listing.
type hostListing struct {
	host     string
	sessions []*serverv1.Session
	// err is why the host did not answer, nil when it did.
	err error
	// staleSince is when the sessions were listed, for a host shown from its cache. Zero for a live one.
	staleSince time.Time
}

// listAllHosts lists this machine and every configured remote, the remotes in parallel, each bounded by
// hostListTimeout. A remote that does not answer is shown from its last listing, if there is one.
func (g *globals) listAllHosts(ctx context.Context, req *serverv1.ListRequest) ([]hostListing, error) {
	if g.remote != "" {
		return nil, errors.New("--all-hosts lists every configured host, so it takes no --remote")
	}
	cfg, err := g.config()
	if err != nil {
		return nil, err
	}
	local, err := cfg.LocalName()
	if err != nil {
		return nil, err
	}
	remotes, err := cfg.Remotes()
	if err != nil {
		return nil, err
	}
	dirs, err := g.dirs()
	if err != nil {
		return nil, err
	}

	out := make([]hostListing, 1+len(remotes))
	var wg sync.WaitGroup
	for i, r := range remotes {
		wg.Go(func() {
			out[i+1] = g.listRemote(ctx, dirs, r, req)
		})
	}

	// This machine as `cm ls` lists it, starting its server if none is running.
	out[0].host = local
	err = withServer(ctx, g, func(ctx context.Context, cl serverv1.ServerClient) error {
		if req.Space != "" {
			if err := requireSpaces(ctx, cl); err != nil {
				return err
			}
		}
		resp, err := cl.List(ctx, req)
		if err != nil {
			return err
		}
		out[0].sessions = resp.Sessions
		return nil
	})
	wg.Wait()
	if err != nil {
		return nil, err
	}

	for i := range out {
		for _, s := range out[i].sessions {
			s.Host = out[i].host
			if !out[i].staleSince.IsZero() {
				s.StaleSinceUnix = out[i].staleSince.Unix()
			}
		}
	}
	markPointedBy(ctx, dirs, out)
	return out, nil
}

// listRemote asks one remote for its sessions, without starting a server there: listing is a question, and
// a server nobody asked for on another machine is not part of the answer. Falls back to the cached listing.
func (g *globals) listRemote(
	ctx context.Context, dirs paths.Dirs, r config.NamedRemote, req *serverv1.ListRequest,
) hostListing {
	listing := hostListing{host: r.Name}
	ctx, cancel := context.WithTimeout(ctx, hostListTimeout)
	defer cancel()

	there := *g
	there.remote = r.Name
	resp, err := func() (*serverv1.ListResponse, error) {
		d, err := there.remoteDialerFor(true)
		if err != nil {
			return nil, err
		}
		conn, cl, err := d.Dial(ctx)
		if err != nil {
			return nil, err
		}
		defer conn.Close()
		// Refused rather than read unfiltered: a server predating spaces ignores the filter and lists every
		// space as though it were the one asked for.
		if req.Space != "" {
			if err := requireServerCapability(serverCapabilities(ctx, cl), capability.Spaces,
				"filter by space"); err != nil {
				return nil, err
			}
		}
		return cl.List(ctx, req)
	}()
	if err == nil {
		listing.sessions = resp.Sessions
		// Cached unfiltered only, so a cached listing never answers a filter it was not taken under.
		if req.Prefix == "" && len(req.Tags) == 0 && req.Space == "" {
			saveListing(dirs, r.Name, resp)
		}
		return listing
	}

	listing.err = err
	if cached, at, ok := loadListing(dirs, r.Name); ok {
		listing.sessions = filterCached(cached.Sessions, req)
		listing.staleSince = at
	}
	return listing
}

func listingPath(dirs paths.Dirs, host string) string {
	return filepath.Join(dirs.State, listingCacheDir, host+".json")
}

// saveListing keeps a remote's listing for when it next does not answer. Best effort: a listing that cannot
// be cached is still a listing, so a failure here is not the command's.
func saveListing(dirs paths.Dirs, host string, resp *serverv1.ListResponse) {
	data, err := protojson.Marshal(resp)
	if err != nil {
		return
	}
	path := listingPath(dirs, host)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	// Renamed into place, so a listing cut short by a full disk or a second command cannot be read half
	// written.
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	_ = os.Rename(tmp, path)
}

// loadListing reads a remote's last listing and when it was taken, the file's modification time.
func loadListing(dirs paths.Dirs, host string) (*serverv1.ListResponse, time.Time, bool) {
	path := listingPath(dirs, host)
	info, err := os.Stat(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, time.Time{}, false
	}
	var resp serverv1.ListResponse
	if err := protojson.Unmarshal(data, &resp); err != nil {
		return nil, time.Time{}, false
	}
	return &resp, info.ModTime(), true
}

// filterCached applies the filters a server would have, so a stale listing answers the same question a live
// one would. Tags are not filtered here, since their selector is the server's grammar: a cached listing asked
// for tags shows nothing rather than everything.
func filterCached(sessions []*serverv1.Session, req *serverv1.ListRequest) []*serverv1.Session {
	if len(req.Tags) > 0 {
		return nil
	}
	var out []*serverv1.Session
	for _, s := range sessions {
		if req.Space != "" && s.Space != req.Space {
			continue
		}
		if req.Prefix != "" && !slices.ContainsFunc(s.Names, func(n string) bool { return strings.HasPrefix(n, req.Prefix) }) {
			continue
		}
		out = append(out, s)
	}
	return out
}

// markPointedBy names, on each remote session, the names of this machine's that point at it. Read from the
// database as following a pointer is, so a missing or newer database marks nothing.
func markPointedBy(ctx context.Context, dirs paths.Dirs, listings []hostListing) {
	if _, err := os.Stat(dirs.Database()); err != nil {
		return
	}
	st, err := store.OpenExisting(ctx, dirs.Database())
	if err != nil {
		return
	}
	defer st.Close()
	pointers, err := st.RemoteBindings(ctx)
	if err != nil || len(pointers) == 0 {
		return
	}
	for _, l := range listings {
		for _, s := range l.sessions {
			for _, p := range pointers {
				if p.Host == l.host && (p.Ref == paths.FormatSessionID(s.Id) || slices.Contains(s.Names, p.Ref)) {
					s.PointedBy = append(s.PointedBy, p.Name)
				}
			}
		}
	}
}

// reportUnanswered says on stderr which hosts did not answer and what was shown for them instead.
func reportUnanswered(listings []hostListing) {
	for _, l := range listings {
		if l.err == nil {
			continue
		}
		if l.staleSince.IsZero() {
			fmt.Fprintf(os.Stderr, "%s: %s did not answer, and no earlier listing is kept: %v\n",
				paths.Name, l.host, l.err)
			continue
		}
		fmt.Fprintf(os.Stderr, "%s: %s did not answer; showing its listing from %s ago: %v\n",
			paths.Name, l.host, humanAge(l.staleSince), l.err)
	}
}

// mergedSessions is every listing's sessions in one slice.
func mergedSessions(listings []hostListing) []*serverv1.Session {
	var out []*serverv1.Session
	for _, l := range listings {
		out = append(out, l.sessions...)
	}
	return out
}
