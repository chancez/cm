package main

import (
	"reflect"
	"testing"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
)

// upgradeCmd builds a command with the flags attach has, parsed from the given argv.
//
// The real attach command is not reused because constructing it needs a globals with directories, and
// what is under test is how set flags become an argv rather than anything attach does with them. The
// flags declared here mirror the ones a client can be started with.
func upgradeCmd(t *testing.T, argv ...string) (*cobra.Command, []string) {
	t.Helper()

	var (
		readOnly   bool
		setTitle   bool
		dir        string
		detachKey  string
		env        []string
		tagArgs    []string
		resumeFrom uint64
	)
	var got []string
	cmd := &cobra.Command{
		Use: "attach",
		RunE: func(_ *cobra.Command, args []string) error {
			got = args
			return nil
		},
	}
	f := cmd.Flags()
	f.BoolVar(&readOnly, "read-only", false, "")
	f.BoolVar(&setTitle, "set-title", true, "")
	f.StringVar(&dir, "dir", "", "")
	f.StringVar(&detachKey, "detach-key", "", "")
	f.StringArrayVar(&env, "env", nil, "")
	f.StringArrayVar(&tagArgs, "tag", nil, "")
	f.Uint64Var(&resumeFrom, resumeFromFlag, 0, "")

	cmd.SetArgs(argv)
	cmd.SetOut(nil)
	if err := cmd.Execute(); err != nil {
		t.Fatalf("parsing %v: %v", argv, err)
	}
	return cmd, got
}

// The argv for a replacement client must name the resolved session and must not carry a position.
//
// The resolved name matters most. A client started with no name had one allocated by the server, so
// re-execing `cm attach` with no name would allocate a *second* session and leave the first orphaned
// with the user's shell still in it. That is the worst outcome this feature could produce, so it is the
// first case here.
//
// The position is the other half, and it is an absence rather than a value. exec makes this argv the
// process's reported command line, and a position is true for one instant, so anything that records a
// live command line ends up replaying one against a stream it does not describe. See resumeEnvVar. Every
// case below therefore sets ResumeFrom and expects no trace of it.
func TestUpgradeArgv(t *testing.T) {
	seq := uint64(4242)

	for _, tc := range []struct {
		name string
		argv []string
		res  client.Result
		want []string
	}{
		{
			// The case that matters: no name was typed, so the resolved one must appear.
			name: "server-allocated name is made explicit",
			argv: nil,
			res:  client.Result{Session: "s7", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{"cm", "attach", "@aaaa2222"},
		},
		{
			// The case this exists for now: the ID is known and the name still wins, so `ps` keeps
			// showing what was typed and a session file saved from the live process keeps a name, which
			// recreates a session where an ID would refuse.
			name: "a typed name is preserved",
			argv: []string{"work"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{"cm", "attach", "work"},
		},
		{
			// A flag taking a value is the case that broke the first implementation. Editing the original
			// argv meant guessing which bare word was the session name, and /tmp here was taken for it.
			name: "a flag value is not mistaken for the session name",
			argv: []string{"--dir", "/tmp"},
			res:  client.Result{Session: "s7", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{"cm", "attach", "@aaaa2222", "--dir=/tmp"},
		},
		{
			// Repeatable flags render as "[a,b]" through Value.String(), which would be passed on as one
			// literal value with brackets in it.
			name: "a repeatable flag becomes one argument each",
			argv: []string{"work", "--env", "A=1", "--env", "B=2"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{
				"cm", "attach", "work",
				"--env=A=1", "--env=B=2",
			},
		},
		{
			// Flags left at their defaults are not emitted, so a default that changes in the new build
			// takes effect instead of being pinned to the old one's value.
			name: "unset flags are omitted",
			argv: []string{"work", "--read-only"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{"cm", "attach", "work", "--read-only=true"},
		},
		{
			// The position goes in the environment, so a live position leaves the argv alone. Without this
			// the recorded command line describes one moment in one window's life forever.
			name: "a position never appears in the argv",
			argv: []string{"work"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{"cm", "attach", "work"},
		},
		{
			// A position an older build put in the argv, or one replayed from a saved session file, is
			// dropped rather than passed on, so it stops spreading at the first upgrade.
			name: "a position left by an older build is dropped",
			argv: []string{"work", "--resume-from-seq=11"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{"cm", "attach", "work"},
		},
		{
			// The command after "--" only matters if the session has to be recreated, and it must stay
			// after the name rather than being read as one.
			name: "a command after a dash is preserved",
			argv: []string{"work", "--", "/bin/sh", "-l"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222", ResumeFrom: &seq},
			want: []string{
				"cm", "attach", "work",
				"--", "/bin/sh", "-l",
			},
		},
		{
			// An ID typed by hand comes back as typed too, rather than being reformatted.
			name: "a typed ID reference is kept",
			argv: []string{"@aaaa2222"},
			res:  client.Result{Session: "work", SessionID: "aaaa2222"},
			want: []string{"cm", "attach", "@aaaa2222"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd, args := upgradeCmd(t, tc.argv...)
			// argv[0] is taken from os.Args so `ps` shows what was launched, which under `go test` is the
			// test binary. Normalized here so the assertion is about the parts this function decides.
			got := upgradeArgv(cmd, args, tc.res)
			if len(got) > 0 {
				got[0] = "cm"
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("upgradeArgv() = %q\nwant %q", got, tc.want)
			}
		})
	}
}
