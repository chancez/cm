package server

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/chancez/cm/internal/capability"
	"github.com/chancez/cm/internal/cmlog"
)

// The server and every attached client are compared with the file the shell running `cm doctor` reads.
//
// The incident: kitty launched from the Dock starts `cm attach` with no XDG_CONFIG_HOME, so those clients
// resolved ~/Library/Application Support/cm/cm.toml, read nothing, and ran on default keys, log_level and
// pager, while `cm config` in a shell reported ~/.config/cm/cm.toml. Nothing looked wrong from the shell,
// which is the one place anyone checks, so the finding has to come from what each process says it read.
func TestCheckConfigMismatch(t *testing.T) {
	dir := t.TempDir()
	shell := filepath.Join(dir, "shell.toml")
	if err := os.WriteFile(shell, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	absent := filepath.Join(dir, "absent.toml")
	const remedy = "; a process started without the shell's environment, such as by a GUI app, " +
		"resolves its own path, so set CM_CONFIG or XDG_CONFIG_HOME where it is launched"

	sess, toks := newSessionWithClients(t, "work", 3)
	sess.noteClientIdentity(toks[0], "v1", 101, capability.Set{})
	sess.noteClientIdentity(toks[1], "v1", 102, capability.Set{})
	sess.noteClientIdentity(toks[2], "v1", 103, capability.Set{})
	sess.noteClientConfig(toks[0], shell)
	sess.noteClientConfig(toks[1], absent)
	// toks[2] says nothing, as an older client would.

	tests := []struct {
		name   string
		server string
		caller string
		want   []Finding
	}{
		{name: "a caller that did not say is skipped", server: absent, caller: ""},
		{
			name: "the server and a client differ", server: absent, caller: shell,
			want: []Finding{
				{
					Kind:   FindingConfigMismatch,
					Detail: "the server reads " + absent + ", which does not exist; this shell reads " + shell + remedy,
				},
				{
					Kind:    FindingConfigMismatch,
					Session: "work",
					Detail:  "client pid 102 reads " + absent + ", which does not exist; this shell reads " + shell + remedy,
				},
			},
		},
		{
			name: "the server agrees", server: shell, caller: shell,
			want: []Finding{{
				Kind:    FindingConfigMismatch,
				Session: "work",
				Detail:  "client pid 102 reads " + absent + ", which does not exist; this shell reads " + shell + remedy,
			}},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			mgr := &Manager{log: cmlog.Discard(), sessions: map[string]*Session{sess.id: sess}}
			mgr.SetConfigPath(tc.server)
			if got := mgr.checkConfigMismatch(tc.caller); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("checkConfigMismatch() =\n%+v\nwant\n%+v", got, tc.want)
			}
		})
	}
}
