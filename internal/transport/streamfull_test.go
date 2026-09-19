package transport_test

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/containerd/ttrpc"

	"github.com/chancez/cm/internal/transport"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// TestASlowConsumerLosesTheStream pins the dependency behavior every stream in cm now has to defend
// against, because it is not obvious from the API and it changed under us.
//
// ttrpc v1.2.7 delivered into an unbuffered channel and blocked, so a consumer that stopped reading
// applied backpressure all the way to the sender. v1.2.9 buffers 64 messages per stream and, if the
// consumer has not drained within a second, closes the stream with ErrStreamFull. Measured below:
// exactly 64 messages arrive, then the stream is dead, and it cannot be resumed.
//
// cm was written against the blocking semantics. Consumers that stall for longer than a second are
// ordinary here -- a terminal that is not draining, the session picker holding the attach loop, a pty
// whose reader has stopped -- so the stall has to be absorbed by cm rather than by the stream. When
// this test fails because a message arrives after the stall, ttrpc has gone back to blocking and the
// absorbing can go with it.
func TestASlowConsumerLosesTheStream(t *testing.T) {
	// os.MkdirTemp rather than t.TempDir: the test name in the path blows the 104-byte cap on darwin.
	dir, err := os.MkdirTemp("", "cmsf")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(dir) })
	sock := filepath.Join(dir, "s.sock")

	srv, err := ttrpc.NewServer()
	if err != nil {
		t.Fatal(err)
	}
	shimv1.RegisterShimService(srv, &floodShim{})
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(context.Background(), ln)
	t.Cleanup(func() { srv.Shutdown(context.Background()) })

	cl, err := transport.DialTTRPC(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cl.Close() })

	sub, err := shimv1.NewShimClient(cl).Subscribe(context.Background(), &shimv1.SubscribeRequest{})
	if err != nil {
		t.Fatal(err)
	}

	// Longer than the one second ttrpc waits before giving up on the stream.
	time.Sleep(1500 * time.Millisecond)

	received := 0
	for {
		if _, err := sub.Recv(); err != nil {
			if !errors.Is(err, ttrpc.ErrStreamFull) {
				t.Fatalf("after stalling, Recv() error = %v, want ErrStreamFull", err)
			}
			// The buffer's size, which is what bounds how much a stalled consumer still gets.
			if received != 64 {
				t.Errorf("messages delivered before the stream was closed = %d, want 64", received)
			}
			return
		}
		received++
		if received > 1000 {
			t.Fatal("the stream survived the stall, so ttrpc blocks again and nothing here is needed")
		}
	}
}

// floodShim sends as fast as the socket accepts, which is what a real shim does when a program inside
// a session writes a burst.
type floodShim struct{}

func (*floodShim) Subscribe(_ context.Context, _ *shimv1.SubscribeRequest, srv shimv1.Shim_SubscribeServer) error {
	for i := 0; ; i++ {
		if err := srv.Send(&shimv1.Output{Seq: uint64(i), Data: []byte("x")}); err != nil {
			return err
		}
	}
}

func (*floodShim) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	return &shimv1.StateResponse{}, nil
}

func (*floodShim) Write(context.Context, *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	return nil, errors.New("unused")
}

func (*floodShim) Resize(context.Context, *shimv1.ResizeRequest) (*shimv1.ResizeResponse, error) {
	return nil, errors.New("unused")
}

func (*floodShim) Signal(context.Context, *shimv1.SignalRequest) (*shimv1.SignalResponse, error) {
	return nil, errors.New("unused")
}

func (*floodShim) Shutdown(context.Context, *shimv1.ShutdownRequest) (*shimv1.ShutdownResponse, error) {
	return nil, errors.New("unused")
}
