package client

import (
	"errors"
	"os"
	"reflect"
	"testing"

	"google.golang.org/protobuf/proto"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/paths"
	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// The new session arriving switches to it, by the reference the caller made.
func TestOverlayNewSessionSwitchesToIt(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("c"))
	if got, want := o.bar(), " cm work | starting a new session... "; got != want {
		t.Fatalf("bar while creating %q, want %q", got, want)
	}
	if got := o.created("@new1", nil); !sameResponse(got, overlayResponse{SwitchTo: "@new1", Repaint: true}) {
		t.Errorf("created = %+v, want a switch to the new session", got)
	}
	if o.mode != overlayClosed {
		t.Errorf("mode %v after the switch, want closed", o.mode)
	}
}

// A failure is said, and a session that arrives after escape is left alone rather than switched to.
func TestOverlayNewSessionFailsOrArrivesLate(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("c"))
	if got := o.created("", errors.New("no shell")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("a failed create = %+v, want nothing done", got)
	}
	if got, want := o.status, "starting a session: no shell"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}

	o, _ = newTestOverlay(t, 24, 80)
	o.open()
	o.feed([]byte("c"))
	o.feed([]byte("\x1b"))
	if got := o.created("@new1", nil); !sameResponse(got, overlayResponse{}) {
		t.Errorf("a session arriving after escape = %+v, want no switch", got)
	}
}

// A read-only client has no business starting a session it would switch to and could not type in.
func TestOverlayNewSessionRefusedReadOnly(t *testing.T) {
	o, _ := newTestOverlay(t, 24, 80)
	o.readOnly = true
	o.open()
	if got := o.feed([]byte("c")); !sameResponse(got, overlayResponse{}) {
		t.Errorf("c read-only = %+v, want nothing asked for", got)
	}
	if got, want := o.status, "a read-only client cannot start a session"; got != want {
		t.Errorf("status %q, want %q", got, want)
	}
}

// The Open keeps what describes the client and drops what described this window's own session: its
// command, tags, persist and on_restore, and the queries its program is waiting on.
func TestNewSessionOpenDropsThisAttachmentsSettings(t *testing.T) {
	opts := Options{
		Session:    "work",
		Command:    []string{"nvim"},
		Dir:        "/client/dir",
		Env:        []string{"A=1"},
		ClientEnv:  map[string]string{"TERM": "xterm-kitty"},
		Persist:    true,
		OnRestore:  "command",
		Tags:       map[string]string{"k": "v"},
		IfRunning:  serverv1.IfRunning_IF_RUNNING_ERROR,
		ConfigPath: "/cfg.toml",
		pending:    &pendingQueries{},
	}
	opts.pending.add([]byte("\x1b[6n"))
	got := newSessionOpen(opts, "/session/dir", 40, 120)
	want := &serverv1.Open{
		Cwd:                "/session/dir",
		CwdExplicit:        true,
		Env:                []string{"A=1"},
		ClientEnv:          map[string]string{"TERM": "xterm-kitty"},
		ClientVersion:      paths.Version(),
		ClientPid:          int32(os.Getpid()),
		ClientConfig:       "/cfg.toml",
		ClientCapabilities: capability.Client().Strings(),
		Rows:               40,
		Cols:               120,
		NoAttach:           true,
		NoRestore:          true,
	}
	if !proto.Equal(got, want) {
		t.Errorf("newSessionOpen = %v, want %v", got, want)
	}
	// And the question is still this attachment's to re-offer, which Open taking it would have lost.
	if got, want := opts.pending.take(), [][]byte{[]byte("\x1b[6n")}; !reflect.DeepEqual(got, want) {
		t.Errorf("pending after building the Open = %q, want %q", got, want)
	}

	// No directory from the session leaves the client's own, as an ordinary attach sends it.
	if got := newSessionOpen(opts, "", 40, 120); got.Cwd != "/client/dir" || got.CwdExplicit {
		t.Errorf("without a session directory, cwd %q explicit %v, want the client's, not explicit",
			got.Cwd, got.CwdExplicit)
	}
}

// Only a local directory is a place to start a session on this machine.
func TestSessionDir(t *testing.T) {
	resp := &serverv1.ListResponse{Sessions: []*serverv1.Session{
		{Id: "a1", Cwd: "/here", CwdIsLocal: true},
		{Id: "b2", Cwd: "/over/ssh"},
	}}
	if got := sessionDir(resp, "a1"); got != "/here" {
		t.Errorf("sessionDir(a1) = %q, want /here", got)
	}
	if got := sessionDir(resp, "b2"); got != "" {
		t.Errorf("sessionDir(b2) = %q, want empty for a session elsewhere", got)
	}
}
