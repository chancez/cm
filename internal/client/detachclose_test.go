package client

import (
	"context"
	"os"
	"testing"
	"time"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

// A client that detaches stays connected until the server has ended the stream.
//
// When the server's Attach handler returns, ttrpc writes the stream's closing status. A client that sent its
// Detach and closed at once made that write fail, ttrpc logged the broken pipe at ERROR, and `cm doctor`
// reported a healthy installation as having a server error for a day.
//
// The stub holds its handler open after the Detach, so a client that does not wait has returned long before
// the handler does. Real time rather than synctest, because the stream runs over a real socket.
func TestDetachWaitsForTheServerToEndTheStream(t *testing.T) {
	const hold = 200 * time.Millisecond

	for name, start := range map[string]func(t *testing.T, socket string) (Result, error){
		// Interrupted, as by SIGTERM: the client detaches on its way out.
		"interrupted": func(t *testing.T, socket string) (Result, error) {
			tty, opts := attachOpts(t, socket)
			ctx, cancel := context.WithCancel(context.Background())
			time.AfterFunc(100*time.Millisecond, cancel)
			return Attach(ctx, tty, opts)
		},
		// The detach key, which is what a person leaving a session presses.
		"detach key": func(t *testing.T, socket string) (Result, error) {
			_, opts := attachOpts(t, socket)
			inR, inW, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { inR.Close(); inW.Close() })
			devNull, err := os.OpenFile(os.DevNull, os.O_WRONLY, 0)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { devNull.Close() })
			tty, err := OpenTTY(inR, devNull)
			if err != nil {
				t.Fatal(err)
			}
			time.AfterFunc(100*time.Millisecond, func() { _, _ = inW.Write([]byte{0x1c}) })
			return Attach(context.Background(), tty, opts)
		},
	} {
		t.Run(name, func(t *testing.T) {
			handlerDone := make(chan struct{})
			svc := &stubService{handle: func(_ int, srv serverv1.Server_AttachServer) error {
				defer close(handlerDone)
				if err := sendOpened(srv, "test", 0); err != nil {
					return err
				}
				for {
					req, err := srv.Recv()
					if err != nil {
						return err
					}
					if req.GetDetach() != nil {
						time.Sleep(hold)
						return nil
					}
				}
			}}
			socket := serveStub(t, svc)

			res, err := start(t, socket)
			if err != nil {
				t.Fatalf("Attach() error = %v", err)
			}
			if !res.Detached {
				t.Fatalf("Attach() = %+v, want a detach", res)
			}
			select {
			case <-handlerDone:
			default:
				t.Errorf("the client returned while the server was still ending the stream, so the server's " +
					"closing write would land on a closed connection")
			}
		})
	}
}
