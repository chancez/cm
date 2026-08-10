package transport

import (
	"errors"
	"io"
	"strings"
)

// IsClosed reports whether an error is the connection having gone away rather than a request failing.
//
// Matched on the message rather than a sentinel, which is ugly and unavoidable: ttrpc carries an error
// across the socket as a status with a string, so nothing comparable survives the trip.
//
// Here rather than in a caller because two of them need it for opposite-looking reasons. The server
// treats it as a shim that exited between accepting a request and replying. A client asking a server to
// shut down treats it as success: Service.Shutdown cannot reply after the fact, since the reply travels
// over the connection that shutdown closes, so a caller racing the teardown sees the socket close
// instead of an answer.
//
// That race is what made `cm server stop` and `cm server restart` report "ttrpc: closed" and exit 1
// after a shutdown that worked. It is timing dependent, so it never appeared on a developer's machine:
// 0 failures in 6 local runs and 0 in 4 under -race, against three separate CI runs where a slower,
// more contended runner lost the race in a different test each time.
func IsClosed(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, io.EOF) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "ttrpc: closed") ||
		strings.Contains(msg, "connection reset") ||
		strings.Contains(msg, "use of closed network connection")
}
