package e2e

import (
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/ansi"
)

// TestImagesSurviveAServerRestart asks whether an image on screen is still there for a client attaching after
// a restart.
//
// cm keeps the payloads a session transmitted, because libghostty's formatter does not re-emit them: a
// restored screen carries placements referring to images by id, and a placement whose image the terminal has
// never seen draws nothing. graphicsRestore prepends the transmissions so the ids resolve, which is what makes
// an image survive a reattach.
//
// That store is built empty by newSession and filled by the pump as commands go past. Adoption rebuilds the
// terminal model by replaying the shim's retained history, so the *model* regains its images, but the replay
// goes straight to the model and not through cm's graphics interception, so cm's own store stays empty. A
// client attaching after a restart would then get placements with nothing to resolve them.
//
// Asserted on the bytes the client receives rather than on anything rendered, which is the only honest thing a
// go test can do here: whether pixels appear is the terminal's business, and what cm controls is whether the
// transmission is in the stream at all.
func TestImagesSurviveAServerRestart(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and a pty, and restarts the server")
	}

	// A one-pixel inline transmission that is also placed, which is the shape `kitten icat` produces: a=T
	// transmits and displays in one command. The payload is recognisable so it can be looked for in a stream.
	const payload = "QUJDQUJD"
	const marker = "IMAGE-SENT"
	script := strings.Join([]string{
		`printf '\033[2J\033[H'`,
		`printf '\033_Ga=T,f=24,s=1,v=1,i=7;` + payload + `\033\\'`,
		`printf '` + marker + `\r\n'`,
		"sleep 60",
	}, "; ")

	e := newEnvWith(t, cmHooksBinary(t), "")

	// The first client, which is what makes the session produce the image at all.
	first := attachOnPty(t, e, "gfxrestart", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, first, marker)

	// It reached this client, so the transmission really happened and the fixture is sound.
	if got := first.output(); !strings.Contains(got, payload) {
		t.Fatalf("the first client never received the transmission, so nothing below is being tested:\n%q",
			got)
	}

	// A fresh attach before any restart: the image is re-sent from cm's store, which is the behaviour this is
	// compared against.
	beforeTranscript := e.state + "/gfxbefore.jsonl"
	before := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + beforeTranscript}, "gfxrestart")
	waitForOnPty(t, before, marker)
	time.Sleep(500 * time.Millisecond)
	if got := string(ansi.SessionBytes(readTranscript(t, beforeTranscript))); !strings.Contains(got, payload) {
		t.Fatalf("a fresh attach without a restart did not carry the image, so the control is broken and the "+
			"restart case below would prove nothing:\n%q", got)
	}
	before.detachKey()
	e.waitFor("the second client to detach", 20*time.Second, func() bool {
		return e.sessionDetail(t, "gfxrestart").Clients == 1
	})

	e.restartServer()
	e.list()
	e.waitFor("the session to be adopted", 25*time.Second, func() bool {
		return e.sessionDetail(t, "gfxrestart").State == "running"
	})

	// And now the same fresh attach, after the restart.
	afterTranscript := e.state + "/gfxafter.jsonl"
	after := attachOnPtyWithEnv(t, e,
		[]string{"CM_TESTHOOK_TRANSCRIPT=" + afterTranscript}, "gfxrestart")
	waitForOnPty(t, after, marker)
	time.Sleep(500 * time.Millisecond)

	if got := string(ansi.SessionBytes(readTranscript(t, afterTranscript))); !strings.Contains(got, payload) {
		t.Errorf("a client attaching after a restart received no image transmission, so the screen it was "+
			"given has placements that resolve to nothing and the image is blank. The model regains its "+
			"images from the history replay, but cm's own store of the payloads does not, and that store is "+
			"what graphicsRestore sends.\nstream:\n%q", got)
	}
}
