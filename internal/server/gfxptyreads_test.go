package server

import (
	"context"
	"encoding/base64"
	"strconv"
	"testing"

	"github.com/chancez/cm/internal/seq"
	"github.com/chancez/cm/internal/vt"
)

// The same image, fed the way a pty delivers it: in reads far smaller than one command.
//
// A pty read caps at 1022 bytes on darwin, so a 4096-byte graphics command arrives across four of them and
// a whole image across hundreds. Every other test here hands the pump whole commands, which is the one
// shape the real thing never produces.
//
// Driven through processChunk rather than a helper, because that is the function the pump calls and it was
// extracted for exactly this: the question is what the ordering inside it does to a command split across
// arrivals.
func TestAnImageArrivingInPtySizedReadsIsStillPlaced(t *testing.T) {
	rec := startShimFor(t, shimConfigFor("gfxptyreads", "sleep 10"))
	term, err := vt.NewSessionTerminal(24, 80, 0)
	if err != nil {
		t.Fatalf("NewSessionTerminal() error = %v", err)
	}
	sess, err := newSession(rec, term, 0, 0)
	if err != nil {
		t.Fatalf("newSession() error = %v", err)
	}
	defer sess.Close()
	if err := sess.Resize(context.Background(), 24, 80, 800, 480); err != nil {
		t.Fatalf("Resize() error = %v", err)
	}

	// 200x120 RGB, chunked at 4096 the way kitty's client chunks.
	const w, h = 200, 120
	px := make([]byte, w*h*3)
	for i := range px {
		px[i] = byte(i % 251)
	}
	enc := base64.RawStdEncoding.EncodeToString(px)

	var stream []byte
	const step = 4096
	first := true
	commands := 0
	for i := 0; i < len(enc); i += step {
		end := min(i+step, len(enc))
		more := "0"
		if end < len(enc) {
			more = "1"
		}
		control := "a=T,q=2,m=" + more
		if first {
			control = "a=T,q=2,f=24,s=" + strconv.Itoa(w) + ",v=" + strconv.Itoa(h) + ",i=7,m=" + more
			first = false
		}
		stream = append(stream, "\x1b_G"+control+";"+enc[i:end]+"\x1b\\"...)
		commands++
	}

	// Delivered in pty-sized reads, which is the variable under test.
	const ptyRead = 1022
	var at seq.Shim
	reads := 0
	for i := 0; i < len(stream); i += ptyRead {
		end := min(i+ptyRead, len(stream))
		sess.processChunk(stream[i:end], at)
		at += seq.Shim(end - i)
		reads++
	}
	t.Logf("%d commands over %d pty-sized reads, %d bytes", commands, reads, len(stream))

	places, err := term.Placements()
	if err != nil {
		t.Fatalf("Placements() error = %v", err)
	}
	rt := sess.gfxStore.Retransmissions()
	t.Logf("model placements = %+v, store images = %d", places, len(rt))

	if len(rt) == 0 {
		t.Error("cm's store kept no image, so a restore carries no transmission")
	}
	if len(places) == 0 {
		t.Fatal("the model holds no placement, so a restore carries no a=p and every client but the one " +
			"that watched it arrive shows a blank space")
	}
	if places[0].ImageID != 7 {
		t.Errorf("placement names image %d, want 7", places[0].ImageID)
	}
}
