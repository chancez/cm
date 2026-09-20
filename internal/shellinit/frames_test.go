package shellinit

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/chancez/cm/internal/osc"
)

// Frames are driven through a real interactive shell on a real pty, which is the only thing that proves
// them.
//
// The hooks all depend on interactivity: zsh runs preexec and precmd only for an interactive shell, bash
// expands PS0 only there, and fish raises fish_preexec only for a command line it was given to run. A
// `shell -c script` run, which is how the report tests work, exercises none of that and would pass with
// every hook uninstalled.
func TestRealShellsEmitFrames(t *testing.T) {
	tests := []struct {
		shell string
		// args start an interactive shell that reads no user configuration, so the test sees this
		// integration rather than whatever is installed on the machine running it.
		args []string
		// source is how that shell loads a file.
		source func(path string) string
	}{
		{
			shell:  "zsh",
			args:   []string{"-f", "-i"},
			source: func(p string) string { return "source " + p + "\n" },
		},
		{
			shell:  "bash",
			args:   []string{"--norc", "--noprofile", "-i"},
			source: func(p string) string { return "source " + p + "\n" },
		},
		{
			shell:  "fish",
			args:   []string{"-N", "-i"},
			source: func(p string) string { return "source " + p + "\n" },
		},
	}

	for _, tt := range tests {
		t.Run(tt.shell, func(t *testing.T) {
			bin, err := exec.LookPath(tt.shell)
			if err != nil {
				t.Skipf("%s is not installed", tt.shell)
			}
			script, err := Script(tt.shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}
			path := writeScript(t, tt.shell, script)

			// The command under test prints its marker in pieces, so the string waited for appears only in
			// its *output* and not in the shell's echo of the line. The first version of this test waited
			// for a marker the echo contained, so it stopped reading before the command had run and passed
			// no frames at all.
			//
			// A second command follows it, because a frame closes at the next prompt on zsh and bash: the
			// first frame is complete only once something after it has run.
			out := driveShell(t, bin, tt.args, tt.source(path),
				"printf 'FRAME%s\\n' ONE\n", "printf 'FRAME%s\\n' TWO\n", "FRAMETWO")

			var tr osc.ReportTracker
			tr.Feed(out)
			frames := tr.TakeFrames()

			// Found by command rather than by position: an interactive shell emits a frame for the `source`
			// line too on some shells, and which ones is not the thing under test.
			var opened, closed *osc.Frame
			for i := range frames {
				f := frames[i]
				switch {
				case !f.Ended && strings.Contains(f.Argv, "FRAME%s"):
					opened = &frames[i]
				case f.Ended && opened != nil && f.ID == opened.ID:
					closed = &frames[i]
				}
			}
			if opened == nil {
				t.Fatalf("%s emitted no frame for the command it ran, frames=%+v\nraw: %q", tt.shell, frames, out)
			}
			if closed == nil {
				t.Errorf("%s never closed frame %q, frames=%+v: an open frame is what strands an announcement",
					tt.shell, opened.ID, frames)
			}
			if !strings.Contains(opened.Argv, "printf") {
				t.Errorf("%s reported argv %q, want the command line it ran", tt.shell, opened.Argv)
			}
		})
	}
}

// The id has to be unique per frame, or a close would end the wrong one and the stack would drift.
func TestRealShellsMintDistinctFrameIDs(t *testing.T) {
	for _, shell := range Shells() {
		t.Run(shell, func(t *testing.T) {
			bin, err := exec.LookPath(shell)
			if err != nil {
				t.Skipf("%s is not installed", shell)
			}
			script, err := Script(shell)
			if err != nil {
				t.Fatalf("Script() error = %v", err)
			}
			path := writeScript(t, shell, script)

			out := driveShell(t, bin, interactiveArgs(shell), "source "+path+"\n",
				"printf 'ID%s\\n' ONE\n", "printf 'ID%s\\n' TWO\n", "printf 'ID%s\\n' THREE\n", "IDTHREE")

			var tr osc.ReportTracker
			tr.Feed(out)
			seen := map[string]bool{}
			for _, f := range tr.TakeFrames() {
				if f.Ended {
					continue
				}
				if seen[f.ID] {
					t.Errorf("%s reused frame id %q", shell, f.ID)
				}
				seen[f.ID] = true
			}
			if len(seen) < 2 {
				t.Skipf("%s emitted %d frames, too few to compare ids", shell, len(seen))
			}
		})
	}
}

// interactiveArgs starts a shell interactively with no user configuration.
func interactiveArgs(shell string) []string {
	switch shell {
	case "zsh":
		return []string{"-f", "-i"}
	case "bash":
		return []string{"--norc", "--noprofile", "-i"}
	default:
		return []string{"-N", "-i"}
	}
}

func writeScript(t *testing.T, shell, script string) string {
	t.Helper()
	dir := t.TempDir()
	name := dir + "/cm-" + shell
	if err := os.WriteFile(name, []byte(script), 0o600); err != nil {
		t.Fatalf("writing the integration: %v", err)
	}
	return name
}

// driveShell runs an interactive shell on a pty, types the lines, and returns everything it printed.
//
// Ends on the marker rather than on a fixed sleep, so a slow machine waits and a fast one does not.
func driveShell(t *testing.T, bin string, args []string, lines ...string) []byte {
	t.Helper()
	if len(lines) < 2 {
		t.Fatal("driveShell needs at least a line to type and a marker")
	}
	marker := lines[len(lines)-1]
	lines = lines[:len(lines)-1]

	cmd := exec.Command(bin, args...)
	// CM_SESSION is what the whole script is gated on, which TestScriptsGuardOnSessionEnv covers from the
	// other side. TERM is dumb so no shell decides to draw anything clever on this pty.
	cmd.Env = append(cmd.Environ(), "CM_SESSION=@test", "TERM=dumb", "PS1=$ ")

	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("starting %s on a pty: %v", bin, err)
	}
	defer func() {
		_ = ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	}()

	var out bytes.Buffer
	done := make(chan struct{})
	go func() {
		defer close(done)
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				out.Write(buf[:n])
				if bytes.Contains(out.Bytes(), []byte(marker)) {
					return
				}
			}
			if err != nil {
				return
			}
		}
	}()

	for _, line := range lines {
		if _, err := io.WriteString(ptmx, line); err != nil {
			t.Fatalf("typing into %s: %v", bin, err)
		}
		// A beat between lines, so the shell has drawn its prompt and installed its hooks before the next
		// one arrives. Without it a shell can read both lines in one go and run them as one command, which
		// is a frame with the wrong argv rather than a failure.
		time.Sleep(200 * time.Millisecond)
	}

	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatalf("%s never printed %q\nraw: %q", bin, marker, out.String())
	}
	// A beat past the marker, so a frame closed at the prompt after it is in the stream too. Without it the
	// reader stops between a command's output and the prompt hook that closes its frame, which reads as a
	// frame that never closes.
	time.Sleep(400 * time.Millisecond)
	return out.Bytes()
}
