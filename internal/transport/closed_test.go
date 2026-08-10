package transport

import (
	"errors"
	"fmt"
	"io"
	"testing"
)

func TestIsClosed(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		// The connection going away. Each spelling has been seen in practice, which is why they are
		// matched by message: ttrpc carries an error across the socket as a status with a string, so no
		// sentinel survives the trip.
		{"ttrpc closed", errors.New("ttrpc: closed"), true},
		{"connection reset", errors.New("connection reset by peer"), true},
		{"closed network connection", errors.New("use of closed network connection"), true},
		{"EOF", io.EOF, true},
		{"wrapped EOF", fmt.Errorf("reading reply: %w", io.EOF), true},
		// Wrapped, because the caller sees these through a call chain rather than bare. This is the form
		// that mattered: `cm server stop` reported "stopping the running server: ttrpc: closed".
		{"wrapped ttrpc closed", fmt.Errorf("stopping the server: %w", errors.New("ttrpc: closed")), true},

		// Real failures, which must not be mistaken for a clean teardown. Treating one of these as
		// success would turn a server that refused to stop into a command that reported it had.
		{"nil", nil, false},
		{"permission denied", errors.New("permission denied"), false},
		{"no such file", errors.New("no such file or directory"), false},
		{"cannot shut down remotely", errors.New("this server cannot be shut down remotely"), false},
		{"unrelated", errors.New("something else went wrong"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsClosed(tt.err); got != tt.want {
				t.Errorf("IsClosed(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}
