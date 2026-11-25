package main

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/chancez/cm/internal/config"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// recordingClient answers List with sessions, or listErr, and records every other request it is sent.
// The embedded interface is nil, so a method nothing here implements panics rather than passing quietly.
type recordingClient struct {
	serverv1.ServerClient
	sessions []*serverv1.Session
	listErr  error
	got      []proto.Message
}

func (c *recordingClient) List(context.Context, *serverv1.ListRequest) (*serverv1.ListResponse, error) {
	if c.listErr != nil {
		return nil, c.listErr
	}
	out := make([]*serverv1.Session, len(c.sessions))
	for i, s := range c.sessions {
		out[i] = proto.Clone(s).(*serverv1.Session)
	}
	return &serverv1.ListResponse{Sessions: out}, nil
}

func (c *recordingClient) Kill(_ context.Context, req *serverv1.KillRequest) (*serverv1.KillResponse, error) {
	c.got = append(c.got, req)
	return &serverv1.KillResponse{Errors: map[string]string{req.Sessions[0]: "refused"}}, nil
}

func (c *recordingClient) Unbind(_ context.Context, req *serverv1.UnbindRequest) (*serverv1.UnbindResponse, error) {
	c.got = append(c.got, req)
	return &serverv1.UnbindResponse{}, nil
}

func testHostSessions(t *testing.T, local, far *recordingClient) *hostSessions {
	t.Helper()
	dir := t.TempDir()
	h := &hostSessions{
		g:       &globals{},
		local:   "here",
		localCl: local,
		dirs:    paths.Dirs{State: dir, Runtime: dir},
		remotes: []config.NamedRemote{{Name: "far"}},
		ctx:     context.Background(),
		conns:   map[string]*hostConn{"far": {cl: far, cancel: func() {}}},
	}
	return h
}

func TestHostSessionsListsEveryHost(t *testing.T) {
	local := &recordingClient{sessions: []*serverv1.Session{{Id: "aaaa1111", Name: "mine"}}}
	far := &recordingClient{sessions: []*serverv1.Session{{Id: "bbbb2222", Name: "job"}}}
	h := testHostSessions(t, local, far)

	resp, err := h.List(context.Background(), &serverv1.ListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	want := []*serverv1.Session{
		{Id: "aaaa1111", Name: "mine", Host: "here"},
		{Id: "bbbb2222", Name: "job", Host: "far"},
	}
	if !equalSessions(resp.Sessions, want) {
		t.Errorf("List() = %v, want %v", resp.Sessions, want)
	}

	// far stops answering: its last listing, which the first List kept, stands in and says how old it is.
	far.listErr = errors.New("gone")
	resp, err = h.List(context.Background(), &serverv1.ListRequest{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(resp.Sessions) != 2 || resp.Sessions[1].Host != "far" || resp.Sessions[1].StaleSinceUnix == 0 {
		t.Errorf("List() with far down = %v, want far:job from its cache, stale", resp.Sessions)
	}
}

// A request goes to the host its reference names, without the host, and what comes back is keyed by the
// reference the picker sent, so it finds its row's result.
func TestHostSessionsRoutesByHost(t *testing.T) {
	local, far := &recordingClient{}, &recordingClient{}
	h := testHostSessions(t, local, far)

	resp, err := h.Kill(context.Background(), &serverv1.KillRequest{Sessions: []string{"far:@bbbb2222"}})
	if err != nil {
		t.Fatalf("Kill() error = %v", err)
	}
	if want := map[string]string{"far:@bbbb2222": "refused"}; !reflect.DeepEqual(resp.Errors, want) {
		t.Errorf("Kill() errors = %v, want %v", resp.Errors, want)
	}
	if _, err := h.Unbind(context.Background(), &serverv1.UnbindRequest{Name: "far:job"}); err != nil {
		t.Fatalf("Unbind() error = %v", err)
	}
	if _, err := h.Unbind(context.Background(), &serverv1.UnbindRequest{Name: "mine"}); err != nil {
		t.Fatalf("Unbind() error = %v", err)
	}

	wantFar := []proto.Message{
		&serverv1.KillRequest{Sessions: []string{"@bbbb2222"}},
		&serverv1.UnbindRequest{Name: "job"},
	}
	wantLocal := []proto.Message{&serverv1.UnbindRequest{Name: "mine"}}
	if !equalMessages(far.got, wantFar) || !equalMessages(local.got, wantLocal) {
		t.Errorf("far got %v, local got %v; want %v and %v", far.got, local.got, wantFar, wantLocal)
	}
}

func equalSessions(a, b []*serverv1.Session) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}

func equalMessages(a, b []proto.Message) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !proto.Equal(a[i], b[i]) {
			return false
		}
	}
	return true
}
