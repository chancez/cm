package e2e

import (
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/fault"
)

// TestAnImagePrintedAtStartupSurvivesForLaterClients holds a new session's adoption open while its pump
// records an image the program printed straight away.
//
// Creating a session goes through adopt, which started the pump and only afterwards replaced the session's
// graphics store with the one its replay built. An image in the first output was recorded into the store
// that was then thrown away: whoever watched live saw it, and every later attach, late push and restore had
// nothing to send. `cm attach work -- kitten icat pic.png` is that case. TestImagesSurviveAServerRestart
// failed about 1 run in 90 on it, as an empty push to a client that answered the probe after its restore.
//
// Ordered by two pauses rather than timing: adopt held right after the pump starts, and the pump held after
// its first chunk, which by then is recorded. Adopt is released first and allowed to finish, so whatever it
// does to the store happens after the image is in it.
func TestAnImagePrintedAtStartupSurvivesForLaterClients(t *testing.T) {
	skipIfShort(t)

	dir := t.TempDir()
	adoptGate := filepath.Join(dir, "adopt")
	pumpGate := filepath.Join(dir, "pump")
	e := newEnvWith(t, cmHooksBinary(t), "",
		"CM_TESTHOOK_FAULTS=after-session-start:pause="+adoptGate+",after-log-append:pause="+pumpGate+
			":count=1")

	// The image is the session's first output, so it is the chunk held at the pump's pause.
	const payload = "QUJDQUJDQUJDQUJD"
	create := e.startInBackground(t, "attach", "--no-attach", "early", "--", "/bin/sh", "-c",
		`printf '\033_Ga=T,f=24,s=2,v=2,i=7;`+payload+`\033\\IMAGE-SENT\r\n'; sleep 60`)

	waitForFile(t, e, "adopt to be held with the pump running", fault.Reached(adoptGate))
	waitForFile(t, e, "the pump to have recorded the image", fault.Reached(pumpGate))
	release(t, adoptGate)
	if err := create.wait(t, 20*time.Second); err != nil {
		t.Fatalf("cm attach --no-attach: %v\nstderr: %s", err, create.errBuf.String())
	}
	release(t, pumpGate)
	e.waitForOutputInSession("early", "IMAGE-SENT", 20*time.Second)

	// A client attaching afterwards gets the image from cm's store, which is the only copy it has: the
	// formatter does not re-emit images.
	later := attachOnPtyDrawing(t, e, nil, "early")
	if got := later.waitForOutput(payload, 15*time.Second); !strings.Contains(got, payload) {
		t.Errorf("a client attaching after the image was printed never received it.\n" +
			"The pump recorded it before adopt finished, so adopt replaced the store that held it.")
	}
}
