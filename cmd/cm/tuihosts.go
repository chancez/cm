package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"google.golang.org/protobuf/proto"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	"github.com/chancez/cm/internal/remote"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// hostRetryAfter is how long a remote that failed to connect is left alone before the picker tries it again,
// shown from its last listing in the meantime. The list refreshes every second, and a host that is
// unreachable rather than slow would otherwise hold each refresh for the whole dial bound.
const hostRetryAfter = 30 * time.Second

// hostSessions is the picker's Sessions over this machine and every remote in [remote.hosts]: `cm tui
// --all-hosts`. It lists them all, each session carrying its host, and sends every other request to the host
// its reference names, which the picker qualifies as host:@id.
//
// One connection per remote for the picker's lifetime rather than one per request, because the list refreshes
// every second and each fresh connection is an ssh. Dialed on first use and dropped on any failure, so a
// remote that comes back is reconnected rather than given up on.
type hostSessions struct {
	g       *globals
	local   string
	localCl serverv1.ServerClient
	dirs    paths.Dirs
	remotes []config.NamedRemote
	// ctx outlives every connection: ssh runs under the context it was dialed with.
	ctx context.Context

	mu    sync.Mutex
	conns map[string]*hostConn
}

type hostConn struct {
	conn   transport.Conn
	cl     serverv1.ServerClient
	cancel context.CancelFunc
	// failedAt and err are set for a remote that did not connect, so it is not dialed again until
	// hostRetryAfter has passed.
	failedAt time.Time
	err      error
}

func newHostSessions(ctx context.Context, g *globals, localCl serverv1.ServerClient) (*hostSessions, error) {
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
	return &hostSessions{
		g: g, local: local, localCl: localCl, dirs: dirs, remotes: remotes, ctx: ctx,
		conns: map[string]*hostConn{},
	}, nil
}

// Close ends every remote connection.
func (h *hostSessions) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for host, c := range h.conns {
		if c.conn != nil {
			c.conn.Close()
			c.cancel()
		}
		delete(h.conns, host)
	}
}

// client returns the connection to host, dialing it if there is none.
func (h *hostSessions) client(host string) (serverv1.ServerClient, error) {
	if host == "" || host == h.local {
		return h.localCl, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.conns[host]; ok {
		if c.cl != nil {
			return c.cl, nil
		}
		if time.Since(c.failedAt) < hostRetryAfter {
			return nil, c.err
		}
	}

	there := *h.g
	there.remote = host
	d, err := there.remoteDialerFor(true)
	if err != nil {
		return nil, err
	}
	// Cancelled only if the dial outlasts its bound, or when the connection is dropped: cancelling sooner
	// would end the ssh the connection runs over.
	ctx, cancel := context.WithCancel(h.ctx)
	timer := time.AfterFunc(hostListTimeout, cancel)
	conn, cl, err := d.Dial(ctx)
	if !timer.Stop() && err == nil {
		conn.Close()
		err = fmt.Errorf("connecting to %s: %w", host, context.DeadlineExceeded)
	}
	if err != nil {
		cancel()
		h.conns[host] = &hostConn{failedAt: time.Now(), err: err}
		return nil, err
	}
	h.conns[host] = &hostConn{conn: conn, cl: cl, cancel: cancel}
	return cl, nil
}

// drop forgets host's connection after a request on it failed, so the next one reconnects.
func (h *hostSessions) drop(host string, err error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if c, ok := h.conns[host]; ok && c.conn != nil {
		c.conn.Close()
		c.cancel()
	}
	h.conns[host] = &hostConn{failedAt: time.Now(), err: err}
}

// route splits a qualified reference into the host's client and the reference there.
func (h *hostSessions) route(ref string) (serverv1.ServerClient, string, string, error) {
	host, rest, ok := remote.SplitRef(ref)
	if !ok {
		return h.localCl, ref, "", nil
	}
	cl, err := h.client(host)
	return cl, rest, host, err
}

// List lists every host, each bounded, a remote that does not answer shown from its last listing.
func (h *hostSessions) List(ctx context.Context, req *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	listings := make([]hostListing, 1+len(h.remotes))
	var wg sync.WaitGroup
	for i, r := range h.remotes {
		wg.Go(func() {
			listings[i+1] = h.listRemote(ctx, r.Name, req)
		})
	}
	resp, err := h.localCl.List(ctx, req)
	wg.Wait()
	if err != nil {
		return nil, err
	}
	listings[0] = hostListing{host: h.local, sessions: resp.Sessions}
	for i := range listings {
		for _, s := range listings[i].sessions {
			s.Host = listings[i].host
			if !listings[i].staleSince.IsZero() {
				s.StaleSinceUnix = listings[i].staleSince.Unix()
			}
		}
	}
	markPointedBy(ctx, h.dirs, listings)
	return &serverv1.ListResponse{Sessions: mergedSessions(listings)}, nil
}

func (h *hostSessions) listRemote(ctx context.Context, host string, req *serverv1.ListRequest) hostListing {
	listing := hostListing{host: host}
	cl, err := h.client(host)
	if err == nil {
		ctx, cancel := context.WithTimeout(ctx, hostListTimeout)
		var resp *serverv1.ListResponse
		resp, err = cl.List(ctx, req)
		cancel()
		if err == nil {
			listing.sessions = resp.Sessions
			if req.Prefix == "" && len(req.Tags) == 0 && req.Space == "" {
				saveListing(h.dirs, host, resp)
			}
			return listing
		}
		h.drop(host, err)
	}
	listing.err = err
	if cached, at, ok := loadListing(h.dirs, host); ok {
		listing.sessions = filterCached(cached.Sessions, req)
		listing.staleSince = at
	}
	return listing
}

// requalify puts the host back on the keys of a per-session result, which the picker reads by the reference
// it sent.
func requalify(host string, m map[string]string) map[string]string {
	if host == "" || len(m) == 0 {
		return m
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		out[host+":"+k] = v
	}
	return out
}

// one routes a request naming exactly one session, which is every request the picker makes: it acts on the
// selected row.
func (h *hostSessions) one(refs []string) (serverv1.ServerClient, []string, string, error) {
	if len(refs) != 1 {
		return nil, nil, "", fmt.Errorf("the picker names one session per request, got %d", len(refs))
	}
	cl, ref, host, err := h.route(refs[0])
	return cl, []string{ref}, host, err
}

func (h *hostSessions) Kill(ctx context.Context, req *serverv1.KillRequest) (*serverv1.KillResponse, error) {
	cl, refs, host, err := h.one(req.Sessions)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.KillRequest)
	r.Sessions = refs
	resp, err := cl.Kill(ctx, r)
	if err != nil {
		return nil, err
	}
	resp.Errors = requalify(host, resp.Errors)
	resp.Unbound = requalify(host, resp.Unbound)
	return resp, nil
}

func (h *hostSessions) Shelve(ctx context.Context, req *serverv1.ShelveRequest) (*serverv1.ShelveResponse, error) {
	cl, refs, host, err := h.one(req.Sessions)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.ShelveRequest)
	r.Sessions = refs
	resp, err := cl.Shelve(ctx, r)
	if err != nil {
		return nil, err
	}
	resp.Errors = requalify(host, resp.Errors)
	return resp, nil
}

func (h *hostSessions) Restore(ctx context.Context, req *serverv1.RestoreRequest) (*serverv1.RestoreResponse, error) {
	cl, refs, host, err := h.one(req.Sessions)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.RestoreRequest)
	r.Sessions = refs
	// The picker's environment is this machine's, which a shell restored elsewhere must not start with.
	if host != "" && host != h.local {
		r.Env = crossHostEnv(os.Environ(), nil, clientHostname())
	}
	resp, err := cl.Restore(ctx, r)
	if err != nil {
		return nil, err
	}
	resp.Errors = requalify(host, resp.Errors)
	return resp, nil
}

func (h *hostSessions) Bind(ctx context.Context, req *serverv1.BindRequest) (*serverv1.BindResponse, error) {
	cl, ref, _, err := h.route(req.Session)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.BindRequest)
	r.Session = ref
	return cl.Bind(ctx, r)
}

func (h *hostSessions) Unbind(ctx context.Context, req *serverv1.UnbindRequest) (*serverv1.UnbindResponse, error) {
	cl, name, _, err := h.route(req.Name)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.UnbindRequest)
	r.Name = name
	return cl.Unbind(ctx, r)
}

func (h *hostSessions) History(ctx context.Context, req *serverv1.HistoryRequest) (*serverv1.HistoryResponse, error) {
	cl, ref, _, err := h.route(req.Session)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.HistoryRequest)
	r.Session = ref
	return cl.History(ctx, r)
}

func (h *hostSessions) Read(ctx context.Context, req *serverv1.ReadRequest) (*serverv1.ReadResponse, error) {
	cl, ref, _, err := h.route(req.Session)
	if err != nil {
		return nil, err
	}
	r := proto.Clone(req).(*serverv1.ReadRequest)
	r.Session = ref
	return cl.Read(ctx, r)
}
