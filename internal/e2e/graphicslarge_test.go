package e2e

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/chancez/cm/internal/ansi"
	"github.com/chancez/cm/internal/vt"
)

// A large image must reach a second client and actually draw there.
//
// Sized like a real one, which is the whole point: the reported case measured restore_bytes=2483667 for a
// screenshot against 28400 for a small image, and only the small one displayed. Everything below 128 KiB of
// base64 goes out as a single command and everything above is chunked, so a small fixture exercises the
// wrong path and passes.
//
// End to end rather than at the seam because the seam is already covered and clean: the assembly, the
// chunking, and libghostty's reception all hold at this size in unit tests. What only a real run covers is
// the wire and the client's write path.
//
// The assertion is a fresh emulator fed what the client received, not a search for the payload in it. A
// restore can carry every byte in the right order and still draw nothing, which is how two bugs shipped
// here already.
func TestALargeImageReachesASecondClient(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns a server, a shell and two ptys, and moves megabytes")
	}

	e := newEnvWith(t, cmHooksBinary(t), "")

	// 800x600 RGB: 1440000 bytes, 1920000 of base64, so about 15 chunks at cm's 128 KiB limit.
	const w, h = 800, 600
	px := make([]byte, w*h*3)
	for i := range px {
		px[i] = byte(i % 251)
	}
	payload := base64.RawStdEncoding.EncodeToString(px)
	cmdPath := filepath.Join(e.state, "image.apc")
	// Chunked at 4096 the way kitty's own client chunks, so this is the shape icat really sends rather
	// than one oversized command. A single command that large trips the scanner's own maxHeld bound,
	// which is a different failure and would mask this one.
	var apc strings.Builder
	const step = 4096
	firstChunk := true
	for i := 0; i < len(payload); i += step {
		end := min(i+step, len(payload))
		more := "1"
		if end == len(payload) {
			more = "0"
		}
		control := "a=T,q=2,m=" + more
		if firstChunk {
			control = "a=T,q=2,f=24,s=" + strconv.Itoa(w) + ",v=" + strconv.Itoa(h) + ",i=7,m=" + more
			firstChunk = false
		}
		apc.WriteString("\x1b_G" + control + ";" + payload[i:end] + "\x1b\\")
	}
	if err := os.WriteFile(cmdPath, []byte(apc.String()), 0o600); err != nil {
		t.Fatalf("writing the image command: %v", err)
	}

	// Emitted by cat rather than printf, because it is megabytes: the program's output path is what
	// matters, not how the bytes get there.
	const marker = "IMAGE-SENT"
	script := "cat " + cmdPath + "; printf '" + marker + "\\r\\n'; sleep 60"

	first := attachOnPty(t, e, "gfxlarge", "--", "/bin/sh", "-c", script)
	waitForOnPty(t, first, marker)
	if got := first.output(); !strings.Contains(got, payload[:64]) {
		t.Fatalf("the first client never received the transmission, so nothing below is being tested")
	}

	// The second client, attaching while the first is still there, which is the reported shape.
	transcript := filepath.Join(e.state, "second.jsonl")
	second := attachOnPtyWithEnv(t, e, []string{"CM_TESTHOOK_TRANSCRIPT=" + transcript}, "gfxlarge")
	waitForOnPty(t, second, marker)
	time.Sleep(1500 * time.Millisecond)

	writes := readTranscript(t, transcript)
	got := ansi.SessionBytes(writes)
	t.Logf("the second client received %d bytes", len(got))

	// Replayed into a fresh model, the way the client's own terminal receives it.
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	defer term.Close()
	if err := term.Resize(24, 80, 10, 20); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}
	if err := term.Write(got); err != nil {
		t.Fatalf("replaying what the client received: %v", err)
	}
	places, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	if len(places) == 0 {
		t.Errorf("the second client's stream drew no image, so it shows a blank space where the picture "+
			"is. transmission present=%v, placement present=%v, %d bytes received",
			strings.Contains(string(got), payload[:64]),
			strings.Contains(string(got), "a=p"), len(got))
	}
}
