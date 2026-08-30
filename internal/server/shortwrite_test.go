package server

import (
	"context"
	"strings"
	"testing"

	"github.com/chancez/cm/internal/cmlog"
	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// truncatingShim reports having written fewer bytes than it was given, with no error.
//
// That is the shape the shim's Write documents: "Short writes are reported rather than retried internally:
// the caller holds the RPC and can decide, and a blocking retry here would stall the whole shim on a wedged
// shell." The caller it defers to is the server, which is what this tests.
type truncatingShim struct {
	shimv1.ShimClient
	keep   int
	writes [][]byte
}

func (t *truncatingShim) Write(_ context.Context, req *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	t.writes = append(t.writes, append([]byte(nil), req.Data...))
	n := t.keep
	if n > len(req.Data) {
		n = len(req.Data)
	}
	return &shimv1.WriteResponse{Written: uint64(n)}, nil
}

// TestAShortWriteToTheShimIsReported covers what happens when the pty takes only part of what was sent.
//
// The shim reports a count and declines to retry, deferring the decision to its caller. Session.Write
// discarded the response entirely, so a partial write looked like a complete one: the keystrokes or the reply
// that did not make it were gone and nothing anywhere said so. For a client's typing that is input silently
// dropped; for a proxied reply it is a program left waiting for an answer cm believes it delivered.
//
// Not reachable through a real pty today, which is why it went unnoticed and why it is tested at the seam.
// os.File.Write loops over write(2) until the buffer is consumed, so a short count with no error does not
// arise from one; the fake is the only way to produce it. That does not make the silence acceptable: the
// shim's contract says a caller has to look, and this one did not.
func TestAShortWriteToTheShimIsReported(t *testing.T) {
	shim := &truncatingShim{keep: 3}
	sess := &Session{shim: shim, log: cmlog.Discard()}

	err := sess.Write(context.Background(), []byte("hello world"))
	if err == nil {
		t.Fatalf("Write() returned nil after the shim accepted 3 of 11 bytes, so 8 bytes of input were "+
			"dropped and the caller was told it succeeded.\nwrites: %q", shim.writes)
	}
	// Named in the error, since a caller that logs it needs to say what was lost rather than "write failed".
	if got := err.Error(); !strings.Contains(got, "3") || !strings.Contains(got, "11") {
		t.Errorf("Write() error = %q, want it to say how much of how much was written", got)
	}
}

// A complete write is unchanged, which is the control: every write cm makes takes this path, so a check that
// rejected a normal one would break the session rather than protect it.
func TestACompleteWriteToTheShimSucceeds(t *testing.T) {
	shim := &truncatingShim{keep: 11}
	sess := &Session{shim: shim, log: cmlog.Discard()}

	if err := sess.Write(context.Background(), []byte("hello world")); err != nil {
		t.Errorf("Write() error = %v on a write the shim accepted in full", err)
	}
}

// An older shim reports nothing at all, and has to keep working.
//
// Written is a wire field, so a shim built before it existed sends zero, which is indistinguishable from
// "wrote nothing". Treating that as a short write would fail every write to such a shim, and a shim is
// re-exec'd from the binary on disk: pairing a new server with older shims is the ordinary state after
// installing a build, not an edge case.
func TestAShimReportingNoCountIsTrusted(t *testing.T) {
	shim := &truncatingShim{keep: 0}
	sess := &Session{shim: shim, log: cmlog.Discard()}

	if err := sess.Write(context.Background(), []byte("hello world")); err != nil {
		t.Errorf("Write() error = %v from a shim that reported no count; an older shim sends zero and "+
			"failing every write to one would break a session after any upgrade", err)
	}
}
