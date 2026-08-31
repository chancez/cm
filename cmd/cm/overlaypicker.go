package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/chancez/cm/internal/paths"
)

// overlayPicker returns the function the overlay uses to hand the terminal to `cm tui`.
//
// A child process for the reason the picker's own attachments are children (docs/tui.md): two programs
// cannot own one terminal, and a child takes its state with it when it exits. The client suspends its
// input reader around this call, which is what makes the handover clean rather than a race for keystrokes.
//
// The chosen session comes back through a file rather than through the child's stdout, and that is not a
// shortcut. stdout *is* the terminal the picker drew on, so anything printed there would land on the
// screen. The alternative, having the picker call the Switch RPC itself, works but has a race worth
// avoiding: the server would push the switch to this client while it is blocked on the child, and the
// reconnect that repaints afterwards would discard the event, so the window would silently not move.
func overlayPicker(g *globals, dirs paths.Dirs) func(context.Context) (string, error) {
	return func(ctx context.Context) (string, error) {
		exe, err := os.Executable()
		if err != nil {
			return "", fmt.Errorf("resolving own path to open the picker: %w", err)
		}

		// In the runtime directory, which is already owner-only, rather than a shared temporary directory:
		// this names a session, and the runtime directory is where cm's other per-session state lives.
		chosen, err := os.CreateTemp(dirs.Runtime, "picked-*")
		if err != nil {
			return "", fmt.Errorf("preparing to hear back from the picker: %w", err)
		}
		path := chosen.Name()
		chosen.Close()
		defer os.Remove(path)

		argv := forwardedDirFlags(g)
		argv = append(argv, "tui", "--chosen-file", path)

		// Not CommandContext, for the reason runAttachChild gives: cancelling kills the child, and a picker
		// killed mid-frame leaves the terminal in whatever mode bubbletea had it in. A signal reaches it
		// through the process group, and bubbletea restores the terminal on the way out.
		_ = ctx
		child := exec.Command(exe, argv...)
		// The terminal, which this process has stopped reading. Stderr too, so a failure the picker prints
		// is visible: unlike an attachment, there is no status line left to carry it.
		child.Stdin, child.Stdout, child.Stderr = os.Stdin, os.Stdout, os.Stderr
		// CM_SESSION would otherwise say the picker is running inside this session, which is true of a
		// command run *in* the session and false of one the client spawned: the picker would print its
		// nesting notice for an attachment that is not nested.
		child.Env = append(os.Environ(), paths.SessionEnv()+"=")

		if err := child.Run(); err != nil {
			return "", fmt.Errorf("running the picker: %w", err)
		}

		// Empty is the ordinary outcome: the picker was quit with q rather than used to switch. The file is
		// created before the child runs, so its absence would be a real failure rather than a choice not
		// made, and either way there is nothing to switch to.
		ref, err := os.ReadFile(path)
		if err != nil {
			return "", nil
		}
		return strings.TrimSpace(string(ref)), nil
	}
}

// forwardedDirFlags repeats the directory and config flags this process was given.
//
// Only the ones actually passed, for the reason attachArgv gives: a resolved default written into a
// child's argv reads as something the user asked for, and that argv is what `ps` shows. Without them a
// sandboxed client would spawn a picker talking to the developer's own server, which is the failure
// AGENTS.md's isolation rule exists to prevent and which would look like the sandbox working.
func forwardedDirFlags(g *globals) []string {
	var argv []string
	if g.runtimeDir != "" {
		argv = append(argv, "--runtime-dir", g.runtimeDir)
	}
	if g.stateDir != "" {
		argv = append(argv, "--state-dir", g.stateDir)
	}
	if g.configPath != "" {
		argv = append(argv, "--config", g.configPath)
	}
	return argv
}

// writeChosenSession records the session the picker's user chose, for the client that opened it.
//
// Written with a rename so the caller cannot read a half-written reference: the caller only looks after
// the child has exited, so this is belt and braces, but a truncated session ID resolves to nothing and the
// window would silently not move.
func writeChosenSession(path, ref string) error {
	tmp := path + ".partial"
	if err := os.WriteFile(tmp, []byte(ref), 0o600); err != nil {
		return fmt.Errorf("recording the chosen session: %w", err)
	}
	if err := os.Rename(tmp, filepath.Clean(path)); err != nil {
		return fmt.Errorf("recording the chosen session: %w", err)
	}
	return nil
}
