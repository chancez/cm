package e2e

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/chancez/cm/internal/ansi"
	"github.com/chancez/cm/internal/graphics"
	"github.com/chancez/cm/internal/paths"
)

// TestClientTerminalReceivesTheProgramsBytesIntact is the end-to-end form of the invariant: a real
// server, a real client on a real pty, and a program emitting the shapes a TUI emits.
//
// It exists because the class of bug it targets is invisible to every other kind of test here. The
// others ask "did the client show FOO", which passes while the bytes around FOO are mangled. This asks
// the two questions that actually matter and that nothing else asked:
//
//   - Did anything cm generated land inside a sequence the program was halfway through writing?
//   - Are the program's own bytes present, in order, once cm's additions are removed?
//
// The fixture is built to make the race deterministic rather than to hope for it. The reported bug
// needed a window title to be published while the session's output was mid-sequence, which happened by
// chance in about a third of attempts. Here the program writes the title and opens a truecolor SGR in
// one write, then sleeps: the metadata round trip measured at 3.8 to 4.1ms, so it lands inside a
// 300ms hole in the middle of an open sequence every time.
//
// The client is built with cm_testhooks so it records a transcript, which is the only view that can
// answer the first question. Reconstructing it from the pty is not enough: what reaches the pty does not
// say which bytes were cm's.
func TestClientTerminalReceivesTheProgramsBytesIntact(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	// The pieces the program writes, in order. Kept as data so the assertion below compares against the
	// same thing the script emits rather than a second copy of it.
	//
	// Every one is a shape that has caused a bug here: a title (the metadata round trip), a truecolor SGR
	// with colon separators split across two writes (the reported case), an OSC 7 carrying a path, a DCS,
	// and an APC, which is the kitty graphics form.
	const (
		titleAndOpenSGR = "\x1b]2;fidelity\x07\x1b[38:2:1"
		closeSGR        = ":2:3m"
		openOSC7        = "\x1b]7;file://host/tmp/fidel"
		closeOSC7       = "ity\x1b\\"
		dcs             = "\x1bP+q4D73\x1b\\"
		apc             = "\x1b_Gf=100;abc\x1b\\"
		tail            = "FIDELITY-DONE"
	)
	// Everything except the graphics APC has to arrive byte for byte. The APC is the one shape cm
	// deliberately does not forward unchanged, and it is checked separately below.
	want := titleAndOpenSGR + closeSGR + openOSC7 + closeOSC7 + dcs

	// Sleeps rather than a single write, so each sequence really is split across two pty reads: cm can
	// only deliver what it read, and a sequence that arrived whole is not the case under test. 0.3s is
	// two orders of magnitude above the round trip it has to lose a race against.
	script := strings.Join([]string{
		"printf '\\033]2;fidelity\\007\\033[38:2:1'",
		"sleep 0.3",
		"printf ':2:3m'",
		"printf '\\033]7;file://host/tmp/fidel'",
		"sleep 0.3",
		"printf 'ity\\033\\\\'",
		"printf '\\033P+q4D73\\033\\\\'",
		"printf '\\033_Gf=100;abc\\033\\\\'",
		"printf 'FIDELITY-DONE'",
		"sleep 30",
	}, "; ")

	// The test-hooks build, which is the only one that records a transcript.
	e := newEnvWith(t, cmHooksBinary(t), "")

	transcript := e.state + "/transcript.jsonl"
	c := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + transcript},
		"fidelity", "--", "/bin/sh", "-c", script)

	// Waits for the program's own last byte rather than a prompt: there is no shell here, the script is
	// the session's command, so the tail is the readiness signal.
	deadline := time.Now().Add(30 * time.Second)
	for !strings.Contains(c.output(), tail) {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %q on the pty, got %q", tail, c.output())
		}
		time.Sleep(50 * time.Millisecond)
	}

	writes := readTranscript(t, transcript)
	if len(writes) == 0 {
		t.Fatalf("the transcript at %s is empty: the client was not built with the %s tag, or the "+
			"hook's env var did not reach it", transcript, paths.TestHooksBuildTag)
	}

	// Question one: did cm interrupt the program?
	if problems := ansi.Validate(writes); len(problems) > 0 {
		for _, p := range problems {
			t.Errorf("%v", p)
		}
		t.Fatalf("cm wrote into the middle of the program's escape sequences, across %d writes",
			len(writes))
	}

	// Question two: are the program's bytes all there, in order? The transcript's session entries are
	// exactly what cm delivered as session output, so this is a byte-for-byte comparison against what the
	// program wrote, with no rendering in between.
	//
	// Compared as a suffix rather than for equality because a fresh attach opens with a serialized screen
	// and cm's own preamble, which are session-tagged and legitimately precede the program's output.
	got := string(ansi.SessionBytes(writes))
	if !strings.Contains(got, want) {
		t.Errorf("the program's bytes did not survive the trip.\nwant to find: %q\nin: %q", want, got)
	}
	if !strings.Contains(got, tail) {
		t.Errorf("the program's output after the graphics command is missing.\nwant: %q\nin: %q", tail, got)
	}

	// The graphics command is the exception, and the exception is deliberate rather than a regression.
	//
	// cm consumes the kitty graphics protocol instead of forwarding it: a transfer naming a file is read
	// and re-emitted inline, because the file is single-use and two readers cannot both have it, and a
	// transmission naming no image is given one, because an image cm cannot name is one it cannot restore
	// to a client that was not there when it was drawn. The program's own bytes are preserved in every way
	// that reaches it: same action, same format, same payload, and an id the terminal would have assigned
	// anyway.
	//
	// So this asserts the semantics rather than the bytes. What it must never do is assert nothing: an
	// empty payload or a lost format key is the corruption this whole test exists to catch.
	i := strings.Index(got, apcIntro)
	if i < 0 {
		t.Fatalf("the graphics command did not arrive at all; got %q", got)
	}
	cmd, _, ok := graphics.Parse([]byte(got[i:]))
	if !ok {
		t.Fatalf("the graphics command did not parse: %q", got[i:])
	}
	if string(cmd.Payload) != "abc" {
		t.Errorf("graphics payload = %q, want %q", cmd.Payload, "abc")
	}
	if cmd.Format != 100 {
		t.Errorf("graphics f= = %d, want 100 preserved from the program", cmd.Format)
	}
	if cmd.ImageID == 0 {
		t.Errorf("cm forwarded an unnamed image, so nothing can place it later: %q", cmd.Raw)
	}
}

// apcIntro is the introducer for a kitty graphics command.
const apcIntro = "\x1b_G"

// cmHooksBinary is cm built with the test-hooks tag, which is what records a transcript.
//
// A separate build from the ordinary one on purpose: a released binary must not contain a hook that
// writes a session's output to a file named by an env var.
var hooksBuildOnce buildResult

func cmHooksBinary(t *testing.T) string {
	return buildCM(t, &hooksBuildOnce, []string{"-tags", paths.TestHooksBuildTag})
}

// attachOnPtyWithEnv is attachOnPty with extra environment for the client process.
//
// Separate rather than a parameter on attachOnPty so the existing callers stay unchanged; the only
// difference is the env, and everything else about driving a client on a pty is the same.
func attachOnPtyWithEnv(t *testing.T, e *env, extraEnv []string, args ...string) *ptyClient {
	t.Helper()

	cmd := exec.Command(e.bin, append([]string{"attach"}, args...)...)
	cmd.Env = append(e.environ(), extraEnv...)
	cmd.Dir = e.state

	ptmx, err := pty.Start(cmd)
	if err != nil {
		t.Fatalf("pty.Start() error = %v", err)
	}
	// Pixel dimensions as well as cells, because a real terminal reports both and cm needs them: the cell
	// size is what lets the model say where an image is, so a pty without them cannot exercise the image
	// restore at all. 80x24 cells at 10x20 pixels.
	if err := pty.Setsize(ptmx, &pty.Winsize{Rows: 24, Cols: 80, X: 800, Y: 480}); err != nil {
		t.Fatalf("Setsize() error = %v", err)
	}

	c := &ptyClient{t: t, ptmx: ptmx, cmd: cmd}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := ptmx.Read(buf)
			if n > 0 {
				c.mu.Lock()
				c.seen.Write(buf[:n])
				c.mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()

	t.Cleanup(func() {
		ptmx.Close()
		_ = cmd.Process.Kill()
		_, _ = cmd.Process.Wait()
	})
	return c
}

// readTranscript loads what the client recorded.
//
// Tolerant of a truncated final line, since the client is killed at cleanup and may be mid-write. A
// partial record is dropped rather than failing the read: the entries before it are still the truth
// about what the terminal received.
func readTranscript(t *testing.T, path string) []ansi.Write {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("opening the transcript: %v", err)
	}
	defer f.Close()

	var writes []ansi.Write
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		var e struct {
			Kind string `json:"kind"`
			Data []byte `json:"data"`
		}
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			continue
		}
		writes = append(writes, ansi.Write{Data: e.Data, Injected: e.Kind == "inject"})
	}
	return writes
}
