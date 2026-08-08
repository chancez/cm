package transport_test

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/chancez/cm/internal/transport"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// These benchmarks measure the transport, not cm.
//
// They deliberately serve a do-nothing implementation of the shim service rather than a real session: the
// question is what the wire costs, and putting a pty, an emulator, and sqlite behind it would bury that in
// noise. A real end-to-end number is a different measurement and belongs in internal/e2e.
//
// Written against the transport package so a second implementation can be measured by changing the dial and
// serve calls and nothing else. That is most of the argument for having the seam at all: it turns "ttrpc is
// the right choice here" from a design claim into something with numbers attached.
//
// Two shapes, because cm's traffic is two shapes. Unary calls are what `list`, `send`, and `report` do, and
// their cost is round-trip latency. Attach is a bidi stream carrying pty output continuously, and its cost
// is throughput at a realistic chunk size.

// benchChunk is the size of one pty read, which bounds a real output message.
//
// Taken from the shim rather than picked: the kernel hands over at most this much per read, so a benchmark
// at 64 KiB would measure a message cm never sends.
const benchChunk = 4096

// benchShim is a shim service that does nothing, so the numbers describe the transport.
type benchShim struct {
	// payload is returned by Subscribe, sized to one pty read.
	payload []byte
	// sends bounds how many messages a Subscribe stream produces before returning.
	sends int
}

func (b *benchShim) State(context.Context, *shimv1.StateRequest) (*shimv1.StateResponse, error) {
	// A response with a few fields set, since an empty message would understate marshalling cost.
	return &shimv1.StateResponse{
		Session: "bench", ShimPid: 1234, ShellPid: 5678,
		NextSeq: 4096, Rows: 24, Cols: 80,
	}, nil
}

func (b *benchShim) Subscribe(
	_ context.Context, _ *shimv1.SubscribeRequest, srv shimv1.Shim_SubscribeServer,
) error {
	for i := range b.sends {
		if err := srv.Send(&shimv1.Output{
			Seq:  uint64(i * len(b.payload)),
			Data: b.payload,
		}); err != nil {
			return err
		}
	}
	return nil
}

func (b *benchShim) Write(context.Context, *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	return &shimv1.WriteResponse{}, nil
}

func (b *benchShim) Resize(context.Context, *shimv1.ResizeRequest) (*shimv1.ResizeResponse, error) {
	return &shimv1.ResizeResponse{}, nil
}

func (b *benchShim) Signal(context.Context, *shimv1.SignalRequest) (*shimv1.SignalResponse, error) {
	return &shimv1.SignalResponse{}, nil
}

func (b *benchShim) Shutdown(
	context.Context, *shimv1.ShutdownRequest,
) (*shimv1.ShutdownResponse, error) {
	return &shimv1.ShutdownResponse{}, nil
}

// serveBench starts a transport server on a fresh socket and returns a connected client.
func serveBench(b *testing.B, svc shimv1.ShimService) shimv1.ShimClient {
	b.Helper()

	// os.MkdirTemp with a short prefix rather than b.TempDir(), which embeds the benchmark name and can
	// exceed the 104-byte sockaddr_un limit. That failure surfaces as a bare EINVAL.
	dir, err := os.MkdirTemp("", "cmb")
	if err != nil {
		b.Fatalf("MkdirTemp() error = %v", err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })
	socket := filepath.Join(dir, "s.sock")

	l, err := net.Listen("unix", socket)
	if err != nil {
		b.Fatalf("Listen() error = %v", err)
	}

	srv, err := transport.NewTTRPCServer()
	if err != nil {
		b.Fatalf("NewTTRPCServer() error = %v", err)
	}
	shimv1.RegisterShimService(srv.Server, svc)

	ctx, cancel := context.WithCancel(context.Background())
	served := make(chan struct{})
	go func() {
		defer close(served)
		_ = srv.Serve(ctx, l)
	}()

	conn, cl, err := transport.DialShim(socket)
	if err != nil {
		cancel()
		b.Fatalf("DialShim() error = %v", err)
	}

	b.Cleanup(func() {
		conn.Close()
		cancel()
		shutdownCtx, stop := context.WithCancel(context.Background())
		stop()
		_ = srv.Shutdown(shutdownCtx)
		<-served
	})
	return cl
}

// BenchmarkUnaryRoundTrip measures one request and response, which is what list, send, and report cost.
func BenchmarkUnaryRoundTrip(b *testing.B) {
	cl := serveBench(b, &benchShim{})
	ctx := context.Background()
	req := &shimv1.StateRequest{}

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := cl.State(ctx, req); err != nil {
			b.Fatalf("State() error = %v", err)
		}
	}
}

// BenchmarkUnaryWithPayload measures a request carrying data, which is what send does with keystrokes.
//
// Separate from the empty round trip so the fixed cost of a call is distinguishable from the cost of the
// bytes in it. A keystroke is a few bytes; a paste is thousands.
func BenchmarkUnaryWithPayload(b *testing.B) {
	for _, size := range []int{8, 1024, benchChunk} {
		b.Run(fmt.Sprintf("%dB", size), func(b *testing.B) {
			cl := serveBench(b, &benchShim{})
			ctx := context.Background()
			req := &shimv1.WriteRequest{Data: make([]byte, size)}

			b.SetBytes(int64(size))
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, err := cl.Write(ctx, req); err != nil {
					b.Fatalf("Write() error = %v", err)
				}
			}
		})
	}
}

// BenchmarkStreamThroughput measures server-to-client streaming at a realistic chunk size.
//
// This is the shape that matters most: a session's output flows over one long-lived stream for its whole
// life, so per-message overhead here is multiplied by everything a shell ever prints.
func BenchmarkStreamThroughput(b *testing.B) {
	// Messages per stream, chosen so the cost of opening one is amortized rather than dominant: a real
	// subscription lasts for the session, not for one message.
	const perStream = 256

	svc := &benchShim{payload: make([]byte, benchChunk), sends: perStream}
	cl := serveBench(b, svc)
	ctx := context.Background()

	b.SetBytes(int64(benchChunk * perStream))
	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sub, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
		if err != nil {
			b.Fatalf("Subscribe() error = %v", err)
		}
		for range perStream {
			if _, err := sub.Recv(); err != nil {
				b.Fatalf("Recv() error = %v", err)
			}
		}
	}
}

// BenchmarkStreamOpen measures opening and closing a stream, separately from moving data through one.
//
// Worth isolating because cm opens a stream per attach, and a client that reconnects after a dropped
// connection pays this each time.
func BenchmarkStreamOpen(b *testing.B) {
	svc := &benchShim{payload: make([]byte, benchChunk), sends: 1}
	cl := serveBench(b, svc)
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		sub, err := cl.Subscribe(ctx, &shimv1.SubscribeRequest{})
		if err != nil {
			b.Fatalf("Subscribe() error = %v", err)
		}
		if _, err := sub.Recv(); err != nil {
			b.Fatalf("Recv() error = %v", err)
		}
	}
}

// BenchmarkRawSocketRoundTrip is the floor: a bare unix socket write and read, no RPC at all.
//
// Included as a control rather than as a comparison anyone should optimize toward. Without it the RPC
// numbers have no scale: knowing a round trip costs 18us means little until you know whether the socket
// itself costs 2us or 17us. It is what separates "the transport is heavy" from "unix sockets cost what they
// cost".
func BenchmarkRawSocketRoundTrip(b *testing.B) {
	dir, err := os.MkdirTemp("", "cmb")
	if err != nil {
		b.Fatalf("MkdirTemp() error = %v", err)
	}
	b.Cleanup(func() { os.RemoveAll(dir) })

	l, err := net.Listen("unix", filepath.Join(dir, "raw.sock"))
	if err != nil {
		b.Fatalf("Listen() error = %v", err)
	}
	b.Cleanup(func() { l.Close() })

	// Echo whatever arrives, which is the least a server can do.
	go func() {
		conn, err := l.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		buf := make([]byte, 64)
		for {
			n, err := conn.Read(buf)
			if err != nil {
				return
			}
			if _, err := conn.Write(buf[:n]); err != nil {
				return
			}
		}
	}()

	conn, err := net.Dial("unix", filepath.Join(dir, "raw.sock"))
	if err != nil {
		b.Fatalf("Dial() error = %v", err)
	}
	b.Cleanup(func() { conn.Close() })

	msg := []byte("ping")
	buf := make([]byte, 64)

	b.ReportAllocs()
	b.ResetTimer()
	for range b.N {
		if _, err := conn.Write(msg); err != nil {
			b.Fatalf("Write() error = %v", err)
		}
		if _, err := conn.Read(buf); err != nil {
			b.Fatalf("Read() error = %v", err)
		}
	}
}
