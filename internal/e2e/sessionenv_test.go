package e2e

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The terminal-describing variables a client reports are applied when the shell is created, not only
// recorded for `cm get-env`.
//
// They were always captured and stored, but only ever served to an already-running shell through a
// prompt hook the user installs themselves. Nothing installs one by default, so a new session's TERM
// was the server's: a session created from kitty reported whatever the shell that started the server
// had.
//
// Through `attach --no-attach` rather than `cm run`, because only an attaching client sends
// ClientEnv. That is deliberate rather than an oversight being worked around: `cm run` has no
// terminal, so it has no terminal state to describe, and a TERM it reported would be its caller's
// rather than any session's. A command that wants a specific value passes --env.
func TestSessionAppliesTheClientsTerminalVariablesAtCreation(t *testing.T) {
	skipIfShort(t)

	e := newEnvWithServerEnv(t, "TERM=server-term")
	e.extraEnv = []string{"TERM=client-term"}

	e.mustRun("attach", "--no-attach", "termcheck")

	// Through a file rather than by matching the session's output.
	//
	// A pty echoes what is sent to it, so the rendered screen holds the command as well as its
	// result, and a needle like "TERM=" matches the echo of the command that was about to produce
	// it. That reads as a pass while proving nothing, which is the single most common way a test
	// here passes for the wrong reason. A file holds only what the shell wrote.
	out := filepath.Join(t.TempDir(), "term")
	e.mustRun("send", "termcheck", "printf %s \"$TERM\" > "+out, "--enter")

	var got string
	e.waitFor("the shell to report its TERM", 30*time.Second, func() bool {
		b, err := os.ReadFile(out)
		got = strings.TrimSpace(string(b))
		return err == nil && got != ""
	})

	if got != "client-term" {
		t.Errorf("session TERM = %q, want the creating client's %q", got, "client-term")
	}
}

// writeScript puts an executable shell script named name in dir.
//
// For tests that need the same program name on two different PATHs, so what runs identifies which
// directory it came from. A plain marker file would not do: the thing being tested is whether the
// shell can find and execute it.
func writeScript(t *testing.T, dir, name, body string) {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatalf("WriteFile(%s) error = %v", path, err)
	}
}

// A session must take its environment from the client that created it, not from the server.
//
// The bug: the server spawns the shim and the shim spawns the shell, both with append(os.Environ(),
// ...), so everything a session had came from whatever shell started the server. A server running
// for days handed every new session a days-old PATH. Observed as sessions created today holding a
// directory removed from the dotfiles the day before, where none of the obvious escapes work:
// `exec zsh -l` re-reads config but still inherits the stale value, `typeset -gU path` only
// reorders, and `cm server restart` from an already-stale shell pins it again.
//
// End to end rather than at the seam, deliberately, even though shimEnv is unit tested. The bug
// lived in the chain between three processes, and the unit test cannot see whether the client
// actually sends what it computed or whether the shim applies what it receives.
func TestSessionInheritsPathFromTheClientNotTheServer(t *testing.T) {
	skipIfShort(t)

	// A directory that exists only for the server, standing in for a PATH entry deleted from the
	// dotfiles after the server started.
	stale := t.TempDir()
	e := newEnvWithServerEnv(t, "PATH="+stale+":/usr/bin:/bin")

	// This client's PATH names a different directory, as a shell in a fresh window would.
	fresh := t.TempDir()
	e.extraEnv = []string{"PATH=" + fresh + ":/usr/bin:/bin"}

	got := e.mustRun("run", "--", "/bin/sh", "-c", "printf %s \"$PATH\"")

	if strings.Contains(got, stale) {
		t.Errorf("session PATH = %q, which still contains the server's %q; a session created "+
			"today is inheriting an environment from whenever the server was started", got, stale)
	}
	if !strings.Contains(got, fresh) {
		t.Errorf("session PATH = %q, which does not contain the creating client's %q", got, fresh)
	}
}

// The shell resolves a bare command name against the inherited PATH, not just report it.
//
// Worth its own test because seeding the environment is not sufficient on its own: exec does not
// resolve a program against cmd.Env, so a fix that only set the variable would leave a command
// resolving against the stale PATH while $PATH read correctly. That split is exactly the kind of
// half-fix that looks right in a printf and fails on the thing a user actually does.
func TestSessionResolvesCommandsAgainstTheClientPath(t *testing.T) {
	skipIfShort(t)

	stale := t.TempDir()
	writeScript(t, stale, "whichbin", "echo from-the-server")
	e := newEnvWithServerEnv(t, "PATH="+stale+":/usr/bin:/bin")

	// Same program name in both, so what runs says which PATH won rather than merely whether one
	// was found.
	fresh := t.TempDir()
	writeScript(t, fresh, "whichbin", "echo from-the-client")
	e.extraEnv = []string{"PATH=" + fresh + ":/usr/bin:/bin"}

	got := e.mustRun("run", "--", "whichbin")

	if !strings.Contains(got, "from-the-client") {
		t.Errorf("running whichbin printed %q, want the client's copy; a bare command name is "+
			"still resolving against the server's PATH", got)
	}
}

// An explicit --env still beats what the session inherits, so the flag keeps meaning what its help
// says. This is a property of ordering, and with the layers reversed the flag is silently shadowed:
// it appears accepted and does nothing.
func TestExplicitEnvBeatsTheInheritedValue(t *testing.T) {
	skipIfShort(t)

	e := newEnvWithServerEnv(t, "PATH="+t.TempDir()+":/usr/bin:/bin")
	e.extraEnv = []string{"PATH=" + t.TempDir() + ":/usr/bin:/bin"}

	chosen := t.TempDir()
	got := e.mustRun("run", "--env", "PATH="+chosen+":/usr/bin:/bin",
		"--", "/bin/sh", "-c", "printf %s \"$PATH\"")

	if !strings.Contains(got, chosen) {
		t.Errorf("session PATH = %q, want the --env value %q", got, chosen)
	}
}

// Inheriting must not widen what reaches the session record on disk. The capture list is an
// allow-list precisely because a developer's environment holds credentials, and a record is a file
// that outlives the session.
func TestInheritingDoesNotWriteTheClientEnvironmentToDisk(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	e.extraEnv = []string{
		"PATH=" + t.TempDir() + ":/usr/bin:/bin",
		"AWS_SECRET_ACCESS_KEY=hunter2",
	}

	name := strings.TrimSpace(e.mustRun("attach", "--no-attach", "creds"))

	// No wait for a prompt: the record is written while the session is being opened, so it is
	// already there. Waiting would also need OSC 133, which a plain /bin/sh never sends.
	//
	// The recorded environment, as `cm get-env` serves it from the store.
	recorded := e.mustRun("get-env", "--all", name)
	for _, banned := range []string{"AWS_SECRET_ACCESS_KEY", "hunter2", "PATH="} {
		if strings.Contains(recorded, banned) {
			t.Errorf("recorded environment contains %q:\n%s", banned, recorded)
		}
	}
}

// An arbitrary variable a client exports reaches the session it creates, which is what makes a
// hand-created session resemble a subshell.
//
// This is the behavior the allow-list version did not have: `FOO=bar cm attach work` used to give a
// session with no FOO, because only PATH and the captured terminal variables travelled.
func TestSessionForwardsAnArbitraryClientVariable(t *testing.T) {
	skipIfShort(t)

	e := newEnvWithServerEnv(t, "SERVER_ONLY=from-server")
	e.extraEnv = []string{"CLIENT_VAR=from-client"}

	got := e.mustRun("run", "--", "/bin/sh", "-c",
		"printf 'client=[%s] server=[%s]' \"$CLIENT_VAR\" \"$SERVER_ONLY\"")

	if !strings.Contains(got, "client=[from-client]") {
		t.Errorf("session did not get the client's variable: %q", got)
	}
	// The server's own environment is still the floor for names the client lacks, so a session
	// created by something with almost no environment still works.
	if !strings.Contains(got, "server=[from-server]") {
		t.Errorf("session lost the server's fallback value: %q", got)
	}
}

// The dynamic linker variables are not forwarded, so a session cannot be handed a preload by the
// client that created it. sshd defaults PermitUserEnvironment to no for this reason.
//
// LD_LIBRARY_PATH rather than LD_PRELOAD or DYLD_INSERT_LIBRARIES, and that choice is the test being
// possible at all. Naming a missing library in either of those kills every process the harness starts:
// dyld refuses to run anything when an inserted dylib cannot be loaded, so the cm client died before
// it could connect and the failure was the harness rather than the assertion. LD_LIBRARY_PATH goes
// through the identical path in shimEnv, since NoInherit does not distinguish between them, and a
// bogus value in it is inert.
func TestSessionDoesNotForwardLinkerVariables(t *testing.T) {
	skipIfShort(t)

	e := newEnv(t)
	e.extraEnv = []string{
		"LD_LIBRARY_PATH=/tmp/should-not-travel",
		"DYLD_LIBRARY_PATH=/tmp/should-not-travel",
		"KEPT=yes",
	}

	got := e.mustRun("run", "--", "/bin/sh", "-c",
		"printf 'ld=[%s] dyld=[%s] kept=[%s]' \"$LD_LIBRARY_PATH\" \"$DYLD_LIBRARY_PATH\" \"$KEPT\"")

	if !strings.Contains(got, "ld=[] dyld=[]") {
		t.Errorf("a linker variable reached the session: %q", got)
	}
	// The control: without this the test would pass even if nothing at all were forwarded.
	if !strings.Contains(got, "kept=[yes]") {
		t.Errorf("forwarding is not happening at all, so the assertion above proves nothing: %q", got)
	}
}
