package e2e

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/graphics"
)

// answerGraphicsProbe makes a pty behave like a terminal that can draw images.
//
// A pty is not a terminal: nothing behind it answers anything, so a client attached to one reports that it
// cannot draw images and is correctly sent none. A test about images therefore has to supply the answer that
// a real kitty would, which is also what makes the negative test meaningful: the two differ by this call and
// nothing else.
//
// Answers once and returns. The probe is sent once per attach, and replying twice would put a second reply
// on the input path with nothing outstanding to consume it.
func answerGraphicsProbe(t *testing.T, c *ptyClient, timeout time.Duration) {
	t.Helper()
	// The query names the id cm probes with, so this waits for cm's own probe rather than for any graphics
	// command a program in the session happened to write.
	want := "i=" + strconv.FormatUint(uint64(graphics.ProbeImageID), 10)
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if got := c.output(); strings.Contains(got, want) && strings.Contains(got, "a=q") {
			c.write([]byte("\x1b_G" + want + ";OK\x1b\\" + "\x1b[?62;c"))
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("no graphics probe arrived within %s, so this test cannot say anything about images", timeout)
}

// attachOnPtyDrawing attaches a client whose pty plays a terminal that can draw images.
//
// Answers as soon as the question appears, which is what a local terminal does. A test about a slow terminal
// answers later on purpose; see the late case below.
func attachOnPtyDrawing(t *testing.T, e *env, extraEnv []string, args ...string) *ptyClient {
	t.Helper()
	c := attachOnPtyWithEnv(t, e, extraEnv, args...)
	answerGraphicsProbe(t, c, 10*time.Second)
	return c
}

// A client whose terminal cannot draw images is sent none of the payload, end to end.
//
// This is the reported bug at full scale and through every hop: a mobile ssh client attached to a session
// holding an image and printed the base64 across its screen. The unit tests hand the server a token that
// already says yes or no, so none of them can catch the wire hop being dropped -- measured by deleting the
// assignment in Service.Attach and watching them all stay green.
//
// The pair is the test. Both clients attach to the same session with the same image on screen, and the only
// difference is that one answers the probe.
func TestGraphicsGoOnlyToATerminalThatAnsweredTheProbe(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and two ptys")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")

	// Exactly the bytes the geometry implies: 2x2 at 24 bits is 12 bytes, which base64 to 16 characters.
	const payload = "QUJDQUJDQUJDQUJD"
	const marker = "IMAGE-SENT"
	script := strings.Join([]string{
		`printf '\033_Ga=T,f=24,s=2,v=2,i=7;` + payload + `\033\\'`,
		`printf '` + marker + `\r\n'`,
		"sleep 60",
	}, "; ")

	// The client that draws the image in the first place, answering the probe as a real terminal would.
	first := attachOnPtyDrawing(t, e, nil, "gfxprobe", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, first, marker)

	// A second client that answers, which must receive the image.
	drawing := attachOnPtyDrawing(t, e, nil, "gfxprobe")
	waitForOnPty(t, drawing, marker)
	time.Sleep(500 * time.Millisecond)
	if got := drawing.output(); !strings.Contains(got, payload) {
		t.Errorf("a client whose terminal answered the probe received no image payload, so the gate is " +
			"stuck shut and no client ever gets an image")
	}
	drawing.detachKey()

	// And one that stays silent, which is every terminal without graphics support. It must get the session
	// and none of the payload.
	quiet := attachOnPty(t, e, "gfxprobe")
	waitForOnPty(t, quiet, marker)
	time.Sleep(500 * time.Millisecond)
	got := quiet.output()
	if strings.Contains(got, payload) {
		t.Errorf("a client whose terminal never answered the probe was sent the image payload, which it "+
			"prints as text: %q", truncateForTest(got))
	}
	// The session itself still has to arrive, or this is a broken attach rather than a withheld image.
	if !strings.Contains(got, marker) {
		t.Errorf("the silent client got no session output either; saw %q", truncateForTest(got))
	}
}

func truncateForTest(s string) string {
	if len(s) > 300 {
		return s[:300] + "..."
	}
	return s
}

// A terminal that answers only after the attach has painted still gets its images.
//
// This is the case the whole design turns on, and the one a blocking probe could not serve. cm asks and does
// not wait, so on a link slower than the answer the attach is served without images; when the answer arrives
// the client reports it and the server sends them then. A fixed wait instead would have had to be long enough
// for the worst link, and any number is too small for some link: kitten icat waits ten seconds for this same
// answer, which is not a wait an attach can afford.
//
// Late is arranged by answering only after the session's own output has appeared, which means Open has already
// been served and the screen already painted.
func TestALateGraphicsAnswerStillGetsImages(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and two ptys")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")

	const payload = "QUJDQUJDQUJDQUJD"
	const marker = "IMAGE-SENT"
	script := strings.Join([]string{
		`printf '\033_Ga=T,f=24,s=2,v=2,i=7;` + payload + `\033\\'`,
		`printf '\r\n` + marker + `\r\n'`,
		"sleep 60",
	}, "; ")

	// The client that draws it, answering promptly so the image exists in the session at all.
	first := attachOnPtyDrawing(t, e, nil, "gfxlate", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, first, marker)

	// The one under test: it attaches, is served a screen with no images because it has said nothing yet,
	// and only then answers.
	late := attachOnPty(t, e, "gfxlate")
	waitForOnPty(t, late, marker)
	if got := late.output(); strings.Contains(got, payload) {
		t.Fatalf("images arrived before the terminal answered, so this test proves nothing about a late " +
			"answer")
	}

	answerGraphicsProbe(t, late, 10*time.Second)
	if got := late.waitForOutput(payload, 15*time.Second); !strings.Contains(got, payload) {
		t.Errorf("a terminal that answered after its attach never received the images, so a link slower " +
			"than the answer loses them permanently")
	}
	// And the placement comes with them, or the payload is bytes for no picture.
	if got := late.output(); !strings.Contains(got, "a=p") {
		t.Error("the late send carried no placement, so nothing draws")
	}
}

// An image drawn while a terminal that cannot draw one is attached does not reach it.
//
// The reported case, and the half a restore gate cannot cover: a kitty and a phone attached to the same
// session, `icat` run in the kitty. It drew there and printed its payload as text across the phone, because a
// session's output goes to every attached client alike and only the restore was gated.
//
// Both clients attach *before* the image exists, which is what makes this the live path rather than the restore
// path, and the image is triggered by typing into the leader the way a user runs icat.
func TestAnImageDrawnLiveSkipsATerminalThatCannotDrawIt(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and two ptys")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")

	const payload = "QUJDQUJDQUJDQUJD"

	// The command goes in a file and is run with cat, so the payload cannot appear on the command line. Typed
	// directly, the shell echoes what was typed and every client sees those characters legitimately: the first
	// version of this test failed on its own echo, which is the needle-in-the-transcript trap docs/testing.md
	// warns about.
	cmdPath := filepath.Join(e.state, "live.img")
	if err := os.WriteFile(cmdPath,
		[]byte("\x1b_Ga=T,f=24,s=2,v=2,i=7;"+payload+"\x1b\\"), 0o600); err != nil {
		t.Fatalf("writing the image command: %v", err)
	}

	// An interactive shell, so the image can be triggered after both clients are watching.
	drawing := attachOnPtyDrawing(t, e, nil, "gfxlive", "--", "/bin/sh")
	waitForOnPty(t, drawing, "$")

	// The one that cannot: attached, and deliberately never answering.
	quiet := attachOnPty(t, e, "gfxlive")
	waitForOnPty(t, quiet, "$")

	// Typed into the leader, which is what running icat is.
	const marker = "IMAGE-DONE"
	drawing.typeLine("cat " + cmdPath + `; printf '\r\n` + marker + `\r\n'`)
	waitForOnPty(t, drawing, marker)
	waitForOnPty(t, quiet, marker)
	// The marker arrives after the image in the same stream, so both clients have been sent everything by
	// now. A sleep here would only be covering for that ordering.

	if got := drawing.output(); !strings.Contains(got, payload) {
		t.Errorf("the terminal that answered the probe never received the live image, so nothing draws "+
			"anywhere: %q", truncateForTest(got))
	}
	if got := quiet.output(); strings.Contains(got, payload) {
		t.Errorf("a terminal that cannot draw images was sent one live, which it prints as base64 across "+
			"the screen: %q", truncateForTest(got))
	}
	// And it is still a working attachment rather than a censored one.
	if got := quiet.output(); !strings.Contains(got, marker) {
		t.Errorf("the quiet client lost the session output as well as the image: %q", truncateForTest(got))
	}
}

// A follower is not censored: `cm read --raw --follow` gets the session's bytes, image included.
//
// The gate is about a *terminal* that would print base64 rather than a picture. A follower paints no terminal,
// it collects the stream, and removing bytes there is corruption in whatever it is writing to. This is the
// guard on the condition rather than on the mechanism: dropping the painting test would make the stripping look
// more uniform and would quietly break every consumer of the byte stream.
func TestAFollowerStillReceivesImages(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")

	const payload = "QUJDQUJDQUJDQUJD"
	cmdPath := filepath.Join(e.state, "follow.img")
	if err := os.WriteFile(cmdPath,
		[]byte("\x1b_Ga=T,f=24,s=2,v=2,i=7;"+payload+"\x1b\\"), 0o600); err != nil {
		t.Fatalf("writing the image command: %v", err)
	}

	drawing := attachOnPtyDrawing(t, e, nil, "gfxfollow", "--", "/bin/sh")
	waitForOnPty(t, drawing, "$")

	// The follower runs for a bounded window; the image is emitted once it is attached, since --follow
	// streams what arrives from now rather than replaying the log.
	got := make(chan string, 1)
	go func() {
		got <- e.followFor(5*time.Second, "read", "--raw", "--follow", "gfxfollow").stdout
	}()
	e.waitFor("the follower to attach", 15*time.Second, func() bool {
		return e.sessionDetail(t, "gfxfollow").Clients == 2
	})
	drawing.typeLine("cat " + cmdPath + "; printf '\r\nFOLLOW-DONE\r\n'")
	waitForOnPty(t, drawing, "FOLLOW-DONE")

	if stream := <-got; !strings.Contains(stream, payload) {
		t.Errorf("a follower did not receive the image, so the byte stream it collects is missing what the "+
			"program wrote: %q", truncateForTest(stream))
	}
}
