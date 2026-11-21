package e2e

import (
	"reflect"
	"testing"
	"time"
)

// startPromptShell starts a zsh that reports OSC 133 and waits for its first prompt, persisting when asked.
//
// Created with attach rather than run, as a terminal's window is: run always captures output, so a session it
// creates has a log to shelve to whether or not it persists.
func (e *env) startPromptShell(name string, persist bool) {
	e.t.Helper()
	args := []string{"attach", "--no-attach", name}
	if persist {
		args = append(args, "--persist")
	}
	args = append(append(args, e.withOSC133()...), "--", "/bin/zsh", "-i")
	e.mustRun(args...)
	e.waitForPrompt(name)
}

// A closed window ends an idle shell, shelving it when it saved output and killing it when it saved none,
// and leaves a busy one running.
//
// End to end because each outcome rests on what a real shell reports: the prompt markers that make it idle,
// and the running command that makes it busy.
func TestCloseFollowsTheShellsState(t *testing.T) {
	skipIfShort(t)
	requireShell(t, "/bin/zsh")
	e := newEnv(t)
	e.writeConfig("[persist]\nenabled = true\n")
	e.restartServer()

	e.startPromptShell("idle", true)
	e.startPromptShell("plain", false)
	e.startPromptShell("busy", true)
	e.mustRun("send", "busy", "sleep 120", "--enter")
	e.waitFor("busy to report its command", 20*time.Second, func() bool {
		s, ok := e.session("busy")
		return ok && s.Busy
	})

	got := map[string]string{}
	for _, name := range []string{"idle", "plain", "busy"} {
		got[name] = e.mustRunWithin(30*time.Second, "close", name)
	}
	want := map[string]string{
		"idle":  "idle: shelved\n",
		"plain": "plain: killed\n",
		"busy":  "busy: detached\n",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cm close printed %v, want %v", got, want)
	}

	states := map[string]string{}
	for _, s := range e.list() {
		states[s.Name] = s.State
	}
	if want := map[string]string{"idle": "shelved", "busy": "running"}; !reflect.DeepEqual(states, want) {
		t.Errorf("states after closing = %v, want %v", states, want)
	}
}
