package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/spf13/cobra"

	"github.com/chancez/cm/internal/client"
)

// routed is what a command's RunE was handed after routing.
type routed struct {
	args   []string
	remote string
	// argv is what an upgrade would re-exec this command as.
	argv []string
}

// runRouted runs a probe command with the given usage line through the real root, with a config naming the
// remote server and this machine laptop.
func runRouted(t *testing.T, use string, args ...string) (routed, error) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "cm.toml")
	body := "[remote]\nlocal_name = \"laptop\"\n[remote.hosts.server]\naddress = \"ssh://me@server.lan\"\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CM_REMOTE", "")
	t.Setenv("CM_CONFIG", "")

	g := &globals{}
	root := newRootCommandFor(g)
	var got routed
	probe := &cobra.Command{
		Use: use,
		RunE: func(cmd *cobra.Command, args []string) error {
			got = routed{
				args:   slices.Clone(args),
				remote: g.remote,
				argv:   upgradeArgv(cmd, args, client.Result{}),
			}
			return nil
		},
	}
	probe.Flags().String("session", "", "")
	root.AddCommand(probe)
	routeBeforeArgs(g, probe)
	root.SetArgs(append([]string{"--config", path, strings.Fields(use)[0]}, args...))
	err := root.Execute()
	return got, err
}

func TestQualifiedRefsRouteToTheirHost(t *testing.T) {
	for _, tc := range []struct {
		name string
		use  string
		args []string
		want routed
	}{
		{
			name: "a remote's session",
			use:  "probe [session]",
			args: []string{"server:@a7k2m9x4"},
			// The re-exec keeps the host, as --remote, beside the plain reference.
			want: routed{
				args:   []string{"@a7k2m9x4"},
				remote: "server",
				argv:   []string{os.Args[0], "probe", "@a7k2m9x4", "--config=CONFIG", "--remote=server"},
			},
		},
		{
			name: "this machine's session",
			use:  "probe [session]",
			args: []string{"laptop:work"},
			want: routed{args: []string{"work"}, argv: []string{os.Args[0], "probe", "work", "--config=CONFIG"}},
		},
		{
			name: "several on one host",
			use:  "probe <session>...",
			args: []string{"server:a", "server:b", "c"},
			want: routed{
				args:   []string{"a", "b", "c"},
				remote: "server",
				argv:   []string{os.Args[0], "probe", "a", "--config=CONFIG", "--remote=server"},
			},
		},
		{
			// Only the session's slot: the rest is a tag whose value has a colon in it.
			name: "a tag value with a colon",
			use:  "probe [session] [key=value]...",
			args: []string{"server:work", "url=http://x"},
			want: routed{
				args:   []string{"work", "url=http://x"},
				remote: "server",
				argv:   []string{os.Args[0], "probe", "work", "--config=CONFIG", "--remote=server"},
			},
		},
		{
			// An optional session left out, so the first argument is not one, and its host is nobody's.
			name: "an unknown host is left alone",
			use:  "probe [session] [key=value]...",
			args: []string{"url=http://x"},
			want: routed{args: []string{"url=http://x"}, argv: []string{os.Args[0], "probe", "url=http://x", "--config=CONFIG"}},
		},
		{
			name: "after the dash is a command",
			use:  "probe [session] -- command",
			args: []string{"server:work", "--", "ssh", "server:path"},
			want: routed{
				args:   []string{"work", "ssh", "server:path"},
				remote: "server",
				argv:   []string{os.Args[0], "probe", "work", "--config=CONFIG", "--remote=server", "--", "ssh", "server:path"},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := runRouted(t, tc.use, tc.args...)
			if err != nil {
				t.Fatalf("Execute() error = %v", err)
			}
			// The config path is a temp dir, so it is compared by position rather than value.
			for i, a := range got.argv {
				if strings.HasPrefix(a, "--config=") {
					got.argv[i] = "--config=CONFIG"
				}
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("routed = %+v\nwant %+v", got, tc.want)
			}
		})
	}
}

func TestQualifiedSessionFlagRoutes(t *testing.T) {
	got, err := runRouted(t, "probe", "--session", "server:work")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.remote != "server" || !slices.Contains(got.argv, "--session=work") {
		t.Errorf("routed = %+v, want remote server and --session=work", got)
	}
}

func TestQualifiedRefsRefuse(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{name: "two hosts", args: []string{"server:a", "laptop:b"}, want: "on one host per command"},
		{name: "a qualifier against --remote", args: []string{"--remote", "ssh://elsewhere", "server:a"}, want: "--remote names ssh://elsewhere"},
		{name: "this machine against --remote", args: []string{"--remote", "server", "laptop:a"}, want: "laptop is this machine"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := runRouted(t, "probe <session>...", tc.args...)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("Execute() error = %v, want one containing %q", err, tc.want)
			}
		})
	}
}

// --remote given as the address of the named host agrees with the qualifier rather than conflicting.
func TestQualifierAgreesWithTheSameHostsAddress(t *testing.T) {
	got, err := runRouted(t, "probe <session>...", "--remote", "ssh://me@server.lan", "server:a")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.remote != "server" {
		t.Errorf("remote = %q, want server", got.remote)
	}
}

// --remote naming this machine is this machine, so one command line works from every host.
func TestRemoteNamingThisMachineIsLocal(t *testing.T) {
	got, err := runRouted(t, "probe [session]", "--remote", "laptop", "work")
	if err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if got.remote != "" {
		t.Errorf("remote = %q, want local", got.remote)
	}
}

// Every command whose usage names a session has its session found, so routing cannot silently skip one.
// Read off the same usage line completion reads, which is the whole guard: a command whose session is not
// spelled there would neither complete nor route.
func TestSessionArgIndexesOfRealCommands(t *testing.T) {
	for key, want := range map[string][]int{
		"attach":       {0},
		"kill":         {0, 1, 2},
		"tag":          {0},
		"space move":   {1, 2},
		"rename":       {1},
		"signal":       {0},
		"send":         {0},
		"restore-with": {0},
	} {
		cmd, ok := runnableCommands(t)[key]
		if !ok {
			t.Fatalf("no command %q", key)
		}
		args := []string{"a", "b", "c"}
		if got := sessionArgIndexes(cmd, args); !reflect.DeepEqual(got, want) {
			t.Errorf("%s (%q): sessionArgIndexes(%q) = %v, want %v", key, cmd.Use, args, got, want)
		}
	}
}

// A command marked as taking a target elsewhere is not routed by it: `cm rebind server:work` moves a window
// of this machine's, so sending it to server asked server to move a window it has never seen.
func TestTargetElsewhereIsNotRouted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cm.toml")
	body := "[remote]\nlocal_name = \"laptop\"\n[remote.hosts.server]\n"
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CM_REMOTE", "")
	g := &globals{}
	root := newRootCommandFor(g)
	var got routed
	probe := &cobra.Command{
		Use:         "probe <session>",
		Annotations: map[string]string{targetElsewhere: "true"},
		RunE: func(cmd *cobra.Command, args []string) error {
			got = routed{args: slices.Clone(args), remote: g.remote}
			return nil
		},
	}
	root.AddCommand(probe)
	routeBeforeArgs(g, probe)
	root.SetArgs([]string{"--config", path, "probe", "server:work"})
	if err := root.Execute(); err != nil {
		t.Fatalf("Execute() error = %v", err)
	}
	if want := (routed{args: []string{"server:work"}}); !reflect.DeepEqual(got, want) {
		t.Errorf("routed = %+v, want %+v", got, want)
	}

	type split struct{ ref, host string }
	g = &globals{configPath: path}
	for in, want := range map[string]split{
		"work":         {"work", ""},
		"laptop:work":  {"work", ""},
		"server:work":  {"work", "server"},
		"server:@abcd": {"@abcd", "server"},
		// Nobody's host is left whole, for the reference check to refuse by name.
		"nowhere:work": {"nowhere:work", ""},
	} {
		ref, host, err := g.splitLocalTarget(in)
		if err != nil || (split{ref, host}) != want {
			t.Errorf("splitLocalTarget(%q) = %q, %q, %v; want %+v", in, ref, host, err, want)
		}
	}
}
