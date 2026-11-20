package main

import (
	"bytes"
	"context"
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/transport"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// olderServer is a server whose build predates shelving: it reports capabilities, none of them shelve.
//
// The embedded interface is nil, so any RPC it does not override panics. That is the assertion that nothing
// reached Shelve or Restore: an older server answers those with Unimplemented, whose message names a method
// rather than the restart that fixes it.
type olderServer struct {
	serverv1.ServerService
}

func (olderServer) Status(context.Context, *serverv1.StatusRequest) (*serverv1.StatusResponse, error) {
	return &serverv1.StatusResponse{
		Capabilities: []string{string(capability.Reported), string(capability.KillExpireAfter)},
	}, nil
}

// serveOlderServer serves olderServer where this process's cm commands look for a server.
//
// The runtime and state directories are fresh ones from os.MkdirTemp, which keeps the socket path under the
// 104-byte limit, and CM_CONFIG names a file that does not exist, so nothing here can reach a developer's
// own server or config.
func serveOlderServer(t *testing.T) {
	t.Helper()
	runtime, err := os.MkdirTemp("", "cmhr")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(runtime) })
	state := t.TempDir()
	t.Setenv("CM_RUNTIME_DIR", runtime)
	t.Setenv("CM_STATE_DIR", state)
	t.Setenv("CM_CONFIG", filepath.Join(state, "absent.toml"))

	g := &globals{}
	dirs, err := g.dirs()
	if err != nil {
		t.Fatal(err)
	}
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	l, err := net.Listen("unix", dirs.ServerSocket())
	if err != nil {
		t.Fatalf("Listen() error = %v", err)
	}
	srv, err := transport.NewTTRPCServer()
	if err != nil {
		t.Fatalf("NewTTRPCServer() error = %v", err)
	}
	serverv1.RegisterServerService(srv.Server, olderServer{})
	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ctx, l)
	}()
	t.Cleanup(func() {
		cancel()
		shutdownCtx, done := context.WithTimeout(context.Background(), 5*time.Second)
		defer done()
		_ = srv.Shutdown(shutdownCtx)
		<-served
	})
}

// `cm shelve` and `cm restore` refuse a server that reports capabilities without shelve, naming the
// restart that fixes it, before sending it anything.
//
// The whole error is compared, since the remedy in it is the point: the binary on disk is already new, and
// the server is the process still running the old one.
func TestShelveAndRestoreRefuseAServerWithoutTheCapability(t *testing.T) {
	serveOlderServer(t)

	for _, tc := range []struct {
		cmd  string
		what string
	}{
		{"shelve", "shelve a session"},
		{"restore", "restore a session"},
	} {
		g := &globals{}
		c := newShelveCommand(g)
		if tc.cmd == "restore" {
			c = newRestoreCommand(g)
		}
		c.SetArgs([]string{"work"})
		c.SetOut(new(bytes.Buffer))
		c.SetErr(new(bytes.Buffer))

		err := c.Execute()
		want := requireServerCapability(capability.Parse([]string{string(capability.Reported)}),
			capability.Shelve, tc.what)
		if err == nil || err.Error() != want.Error() {
			t.Errorf("cm %s against an older server: error = %v\nwant %v", tc.cmd, err, want)
		}
	}
}

// --expire-after is checked before anything is asked of a server, the way kill checks it.
func TestShelveValidatesExpireAfter(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"--expire-after", "0s", "work"}, `--expire-after: expiry must be positive or never, got "0s"`},
		{[]string{"--expire-after", "soon", "work"}, `--expire-after: expiry "soon": want a Go duration such as 720h, or never`},
		{[]string{"--tag", "k=v", "work"}, "--tag takes no session names"},
	} {
		cmd := newShelveCommand(&globals{})
		cmd.SetArgs(tc.args)
		cmd.SetOut(new(bytes.Buffer))
		cmd.SetErr(new(bytes.Buffer))
		if err := cmd.Execute(); err == nil || err.Error() != tc.want {
			t.Errorf("Execute(%q) error = %v, want %q", tc.args, err, tc.want)
		}
	}
}

// The attach notice names any ended session that comes back as it was, by any of its references and with its
// state, and says nothing for one that saved nothing, a running one, or a name nothing holds.
func TestIgnoredCommandNotice(t *testing.T) {
	sessions := []*serverv1.Session{
		{Name: "work", Id: "a7k2m9x4", Names: []string{"work", "agent"},
			State: serverv1.SessionState_SESSION_STATE_SHELVED, Exited: true, Restorable: true},
		{Name: "crashed", Id: "c7k2m9x4", Names: []string{"crashed"},
			State: serverv1.SessionState_SESSION_STATE_DEAD, Exited: true, Restorable: true},
		{Name: "done", Id: "d7k2m9x4", Names: []string{"done"},
			State: serverv1.SessionState_SESSION_STATE_EXITED, Exited: true, Restorable: true},
		{Name: "plain", Id: "e7k2m9x4", Names: []string{"plain"},
			State: serverv1.SessionState_SESSION_STATE_EXITED, Exited: true},
		{Name: "live", Id: "f7k2m9x4", Names: []string{"live"},
			State: serverv1.SessionState_SESSION_STATE_RUNNING},
	}
	var got []string
	for _, ref := range []string{"work", "agent", "@a7k2m9x4", "crashed", "done", "plain", "live", "missing"} {
		got = append(got, ignoredCommandNotice(sessions, ref))
	}
	want := []string{
		"cm: work is shelved and comes back as it was; ignoring the command after --",
		"cm: agent is shelved and comes back as it was; ignoring the command after --",
		"cm: @a7k2m9x4 is shelved and comes back as it was; ignoring the command after --",
		"cm: crashed is dead and comes back as it was; ignoring the command after --",
		"cm: done is exited and comes back as it was; ignoring the command after --",
		"",
		"",
		"",
	}
	if !slices.Equal(got, want) {
		t.Errorf("notices = %q\nwant %q", got, want)
	}
}
