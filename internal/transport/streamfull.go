package transport

import (
	"errors"
	"strings"

	"github.com/containerd/ttrpc"
)

// IsStreamFull reports whether an error is ttrpc having given up on a stream because its consumer fell
// behind, rather than the stream ending.
//
// The distinction is the whole point. ttrpc v1.2.9 buffers 64 messages per stream and closes it after a
// second if the consumer has not drained; v1.2.7 blocked instead, so every consumer in cm was written
// against backpressure that no longer exists. A consumer that reads this as the stream ending draws the
// wrong conclusion from it: the server's pump concluded that a live session had finished, asked the shim
// for an exit status it did not have, and recorded exit code -1 while the shell carried on.
//
// Here rather than beside each caller so ttrpc stays inside this package, as with IsClosed, and matched
// on the message as well as the sentinel for the same reason: a status crossing the socket keeps the
// string and nothing else.
func IsStreamFull(err error) bool {
	if err == nil {
		return false
	}
	return errors.Is(err, ttrpc.ErrStreamFull) ||
		strings.Contains(err.Error(), "ttrpc: stream buffer full")
}
