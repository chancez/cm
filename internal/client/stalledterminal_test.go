package client

import (
	"bytes"
	"context"
	"os"
	"sync"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A terminal that stops draining for longer than a second must not cost the client its stream.
//
// The reported symptom: two sessions reconnected 33ms apart with the server never having restarted, and
// `ttrpc: failed to handle message error="ttrpc: stream buffer full"` printed into one of them. One kitty
// that stops reading stalls every cm client in every one of its windows at once, which is what made two
// unrelated sessions fail together.
//
// The cause is in the dependency: ttrpc v1.2.9 buffers 64 messages per stream and closes it after a
// second, where v1.2.7 blocked. So the client absorbs the stall in outQueue instead of leaving the stream
// waiting. See TestASlowConsumerLosesTheStream in internal/transport for the measurement.
//
// Driven by blocking the terminal rather than by holding the loop, because that is the reported case and
// it needs no test hook: an undrained pipe blocks the write once its buffer fills, exactly as a terminal
// that is not reading does.
func TestAStalledTerminalKeepsTheStream(t *testing.T) {
	// Both bounds matter, and the first version of this test had only one of them, so it passed with the
	// fix reverted.
	//
	// More messages than the buffers ahead of the queue can hold, or nothing overflows and the stall is
	// absorbed by ttrpc's 64 slots: at 65 messages this test proved nothing. And fewer bytes than
	// maxOutBacklog, or the queue drops them and the client repaints, which is correct behavior but not
	// what is under test here.
	const (
		chunk  = 2 << 10
		chunks = 300
	)
	payload := bytes.Repeat([]byte{'z'}, chunk)
	// Distinctive, and sent last: if the stream was lost the reconnect gets a fresh handler and this
	// never arrives.
	tail := []byte("STREAM-SURVIVED")

	svc := &stubService{handle: func(n int, srv serverv1.Server_AttachServer) error {
		if n > 1 {
			// A second connection means the stream was lost. Ended immediately so the test does not hang
			// waiting for output that a reconnected client would be sent instead.
			return srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: 0}},
			})
		}
		if err := sendOpened(srv, "test", 0); err != nil {
			return err
		}
		for i := range chunks {
			out := &serverv1.Output{Seq: uint64(i * chunk), Data: payload}
			if err := srv.Send(&serverv1.AttachResponse{
				Event: &serverv1.AttachResponse_Output{Output: out},
			}); err != nil {
				return err
			}
		}
		if err := srv.Send(&serverv1.AttachResponse{
			Event: &serverv1.AttachResponse_Output{Output: &serverv1.Output{
				Seq: uint64(chunks * chunk), Data: tail,
			}},
		}); err != nil {
			return err
		}
		return srv.Send(&serverv1.AttachResponse{
			Event: &serverv1.AttachResponse_Exited{Exited: &serverv1.Exited{ExitCode: 0}},
		})
	}}
	socket := serveStub(t, svc)

	tty, opts, written := stalledTTY(t, socket, 1500*time.Millisecond)

	result, err := Attach(context.Background(), tty, opts)
	if err != nil {
		t.Fatalf("Attach() error = %v", err)
	}

	if n := svc.connections(); n != 1 {
		t.Errorf("made %d connections, want 1: a terminal that stopped draining must not cost the stream",
			n)
	}
	if !result.Exited {
		t.Errorf("result = %+v, want the exit observed on the first connection", result)
	}
	got := written()
	if !bytes.Contains(got, tail) {
		t.Errorf("the terminal received %d bytes and not the last chunk, want everything sent while it "+
			"was blocked", len(got))
	}
	if n := bytes.Count(got, []byte{'z'}); n != chunk*chunks {
		t.Errorf("the terminal received %d payload bytes, want %d", n, chunk*chunks)
	}
}

// stalledTTY is a TTY whose output is not read for stall, and read normally afterwards. The returned
// function reports everything written, once the attachment has finished.
func stalledTTY(t *testing.T, socket string, stall time.Duration) (*TTY, Options, func() []byte) {
	t.Helper()

	inR, inW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}
	inW.Close()
	outR, outW, err := os.Pipe()
	if err != nil {
		t.Fatalf("Pipe() error = %v", err)
	}

	var (
		mu   sync.Mutex
		seen bytes.Buffer
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		time.Sleep(stall)
		buf := make([]byte, 4096)
		for {
			n, err := outR.Read(buf)
			if n > 0 {
				mu.Lock()
				seen.Write(buf[:n])
				mu.Unlock()
			}
			if err != nil {
				return
			}
		}
	}()
	t.Cleanup(func() {
		inR.Close()
		outR.Close()
	})

	tty, err := OpenTTY(inR, outW)
	if err != nil {
		t.Fatalf("OpenTTY() error = %v", err)
	}
	written := func() []byte {
		// Closed so the reader sees EOF and stops, which is what makes the buffer final rather than a
		// snapshot of a race.
		outW.Close()
		<-done
		mu.Lock()
		defer mu.Unlock()
		return append([]byte(nil), seen.Bytes()...)
	}
	return tty, Options{Session: "test", SocketPath: socket}, written
}
