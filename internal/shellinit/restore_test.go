package shellinit

import (
	"bytes"
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/osc"
)

// A revived shell runs or offers the command cm handed it in CM_RESTORE_COMMAND, and says so before its first
// prompt, so the server does not type it in as well.
//
// Run: the line runs at the first prompt inside a frame whose argv is the line, which is what the carried
// declaration binds to and what records it as the typed command again. Offered: it is in the line editor, so
// text typed next extends it, and the edited line is what runs. bash cannot fill the editor from a hook, so it
// does not take an offered command, and the server types that one in; neither does fish 3, and bash before
// 4.4 takes none. Either way the variables are gone by the time anything the shell runs looks for them.
func TestRealShellsTakeTheRestoreCommand(t *testing.T) {
	const line = `printf 'RAN%s\n' OK`
	for _, shell := range Shells() {
		for _, run := range []bool{true, false} {
			name := shell + "/offer"
			if run {
				name = shell + "/run"
			}
			t.Run(name, func(t *testing.T) {
				bin, err := exec.LookPath(shell)
				if err != nil {
					t.Skipf("%s is not installed", shell)
				}
				script, err := Script(shell)
				if err != nil {
					t.Fatalf("Script() error = %v", err)
				}
				path := writeScript(t, shell, script)
				flag := "CM_RESTORE_RUN=0"
				if run {
					flag = "CM_RESTORE_RUN=1"
				}
				takes := takesRestore(t, shell, bin, run)
				printEnv := `printf 'ENV%s\n' "[${CM_RESTORE_COMMAND-}${CM_RESTORE_RUN-}]"` + "\n"
				if shell == "fish" {
					printEnv = `printf 'ENV%s\n' "[$CM_RESTORE_COMMAND$CM_RESTORE_RUN]"` + "\n"
				}

				// The first typed line finishes an offered command and is a command of its own otherwise. The
				// second prints what is left of the variables, in pieces, so the marker is only in its output.
				out := driveShellEnv(t, bin, interactiveArgs(shell), []string{"CM_RESTORE_COMMAND=" + line, flag},
					"source "+path+"\n",
					" EDITED\n",
					printEnv,
					"ENV[]")

				if got := bytes.Contains(out, []byte(osc.RestoreTaken)); got != takes {
					t.Errorf("%s reported restore=taken = %v, want %v\nraw: %q", shell, got, takes, out)
				}
				var tr osc.ReportTracker
				tr.Feed(out)
				var argvs []string
				for _, f := range tr.TakeFrames() {
					if !f.Ended && strings.HasPrefix(f.Argv, "printf 'RAN") {
						argvs = append(argvs, f.Argv)
					}
				}
				var want []string
				switch {
				case takes && run:
					want = []string{line}
				case takes:
					want = []string{line + " EDITED"}
				}
				if !slices.Equal(argvs, want) {
					t.Errorf("%s ran frames %q, want %q\nraw: %q", shell, argvs, want, out)
				}
				if takes && run && !bytes.Contains(out, []byte("RANOK")) {
					t.Errorf("%s never ran its restore command\nraw: %q", shell, out)
				}
				if !run && takes && !bytes.Contains(out, []byte("RANEDITED")) {
					t.Errorf("%s did not put its restore command in the line editor\nraw: %q", shell, out)
				}
			})
		}
	}
}

// A restored command interrupted with ctrl-c closes its frame, as one the user typed does.
//
// The shells run it from a prompt hook, and an interrupt there aborts the hook in zsh and fish, so the close
// after it never ran: the session went on reporting the command as running, kept it as the typed command, and
// a revive brought back a program the user had quit.
func TestRealShellsCloseAnInterruptedRestoreCommandsFrame(t *testing.T) {
	for _, shell := range Shells() {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			if !takesRestore(t, shell, bin, true) {
				t.Skipf("%s leaves its restore command to cm to type in", shell)
			}
			script, err := Script(shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}
			path := writeScript(t, shell, script)
			// The empty lines are pauses, so sleep is running by the time ctrl-c arrives.
			out := driveShellEnv(t, bin, interactiveArgs(shell),
				[]string{"CM_RESTORE_COMMAND=sleep 30", "CM_RESTORE_RUN=1"},
				"source "+path+"\n", "", "", "\x03", "printf 'DONE%s\\n' X\n", "DONEX")

			var tr osc.ReportTracker
			tr.Feed(out)
			var sleep []osc.Frame
			id := ""
			for _, f := range tr.TakeFrames() {
				if f.Argv == "sleep 30" {
					id = f.ID
				}
				if id != "" && f.ID == id {
					sleep = append(sleep, f)
				}
			}
			want := []osc.Frame{{ID: id, Argv: "sleep 30"}, {ID: id, Ended: true}}
			if id == "" || !slices.Equal(sleep, want) {
				t.Errorf("%s frames for the interrupted command = %+v, want %+v\nraw: %q", shell, sleep, want, out)
			}
		})
	}
}

// A restored command runs between the shell's own preexec and precmd hooks, everyone's rather than only cm's,
// as a command the user entered does.
//
// Those hooks are where kitty and starship send OSC 133;C, which is how cm knows a command is running, and
// where a title hook names the window. Run without them, `cm list` showed a restored program as nothing and
// `cm wait --until idle` returned while it ran. Each shell is given hooks of another integration's, installed
// before cm's script as kitty's are, and they must report the command around its output.
func TestRealShellsRunARestoreCommandThroughTheirHooks(t *testing.T) {
	const line = `printf 'RAN%s\n' OK`
	// Each prints its marker in pieces, so the echo of the line that installs it cannot satisfy the test.
	hooks := map[string]string{
		"zsh": `_t_pre() { print -r -- "PR""E:$1" }; _t_post() { print -r -- "PO""ST" }; ` +
			`preexec_functions+=(_t_pre); precmd_functions+=(_t_post)` + "\n",
		"bash": `PS0='PR''E:\n'` + "\n",
		"fish": `function _t_pre --on-event fish_preexec; echo "PR""E:$argv"; end; ` +
			`function _t_post --on-event fish_postexec; echo "PO""ST"; end` + "\n",
	}
	for _, shell := range Shells() {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			if !takesRestore(t, shell, bin, true) {
				t.Skipf("%s leaves its restore command to cm to type in", shell)
			}
			script, err := Script(shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}
			path := writeScript(t, shell, script)
			out := string(driveShellEnv(t, bin, interactiveArgs(shell),
				[]string{"CM_RESTORE_COMMAND=" + line, "CM_RESTORE_RUN=1"},
				hooks[shell], "source "+path+"\n", "", "printf 'DONE%s\\n' X\n", "DONEX"))

			ran := strings.Index(out, "RANOK")
			if ran < 0 {
				t.Fatalf("%s never ran its restore command\nraw: %q", shell, out)
			}
			// bash's PS0 is not given the line. It is expanded once for the source line typed before, and must
			// be again for the restored command.
			if shell == "bash" {
				if n := strings.Count(out[:ran], "PRE:"); n != 2 {
					t.Errorf("bash expanded PS0 %d times before the command's output, want 2\nraw: %q", n, out)
				}
			} else if pre := strings.Index(out, "PRE:"+line); pre < 0 || pre > ran {
				t.Errorf("%s: preexec hook for the line at %d, its output at %d; want the hook first\nraw: %q",
					shell, pre, ran, out)
			}
			// bash's prompt hooks run when the prompt after it is drawn, as they do for any command.
			if shell != "bash" && !strings.Contains(out[ran:], "POST") {
				t.Errorf("%s: no precmd or postexec hook after the command\nraw: %q", shell, out)
			}
		})
	}
}
