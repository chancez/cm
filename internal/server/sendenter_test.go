package server

import (
	"context"
	"slices"
	"strings"
	"testing"
	"testing/synctest"

	shimv1 "github.com/chancez/cm/proto/cm/shim/v1"
)

// recordingShim captures each Write separately, which is the whole point: the bug was two logical
// writes arriving as one, and a fixture that concatenated them could not tell the difference.
type recordingShim struct {
	shimv1.ShimClient
	writes [][]byte
}

func (r *recordingShim) Write(_ context.Context, req *shimv1.WriteRequest) (*shimv1.WriteResponse, error) {
	r.writes = append(r.writes, append([]byte(nil), req.Data...))
	return &shimv1.WriteResponse{}, nil
}

// A send's text and its submitting keypress reach the pty as two separate writes.
//
// The bug: `cm send <session> '<long text>' --enter` concatenated the CR onto the text, so both went out
// in one pty write. A pty read returns at most 1022 bytes, so a large write arrives as several reads
// (1201 bytes measured as [1022, 179]), and a full-screen program doing paste detection treats that burst
// as a paste, consuming the trailing CR as pasted content rather than as the key that submits it.
//
// Reported driving a Claude Code session: the prompt appeared in its input box as "[Pasted text #4]" and
// sat there until a second `cm send --key enter` arrived. Measured against a real one with only the length
// varying: 42 bytes submitted, 121 and 281 bytes landed without submitting, 842 bytes did not appear at all
// until a separate enter, and two writes submitted at every size.
//
// Asserted as the whole sequence of writes rather than by looking for a CR somewhere, because "the CR was
// sent" was already true when the bug existed. What matters is that it was not in the same write as the
// text.
func TestSendWritesEnterSeparately(t *testing.T) {
	long := strings.Repeat("x", 1200)

	tests := []struct {
		name  string
		data  string
		enter string
		want  [][]byte
	}{
		{
			name: "long text with enter is two writes",
			data: long, enter: "\r",
			want: [][]byte{[]byte(long), []byte("\r")},
		},
		{
			name: "short text with enter is also two writes",
			data: "make", enter: "\r",
			want: [][]byte{[]byte("make"), []byte("\r")},
		},
		{
			// No enter means one write, so `cm send --key ctrl-c` and a bare text send are unchanged.
			name: "text without enter is one write",
			data: "make", enter: "",
			want: [][]byte{[]byte("make")},
		},
		{
			// A keys-only send, such as `cm send s --key enter`, must still write the key.
			name: "enter with no text is one write",
			data: "", enter: "\r",
			want: [][]byte{[]byte("\r")},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// synctest so enterDelay costs no real time and the wait is deterministic rather than a
			// sleep the suite pays for on every send case.
			synctest.Test(t, func(t *testing.T) {
				rec := &recordingShim{}
				sess := &Session{shim: rec}

				if err := writeInputThenEnter(context.Background(), sess, []byte(tt.data), []byte(tt.enter)); err != nil {
					t.Fatalf("writeInputThenEnter() error = %v", err)
				}

				if !slices.EqualFunc(rec.writes, tt.want, func(a, b []byte) bool { return string(a) == string(b) }) {
					got := make([]string, 0, len(rec.writes))
					for _, w := range rec.writes {
						got = append(got, truncate(string(w)))
					}
					want := make([]string, 0, len(tt.want))
					for _, w := range tt.want {
						want = append(want, truncate(string(w)))
					}
					t.Errorf("writes = %q, want %q.\nA single write holding both the text and the CR is the bug: "+
						"a pty splits it at 1022 bytes and the reader treats the burst as a paste, so the CR never "+
						"submits.", got, want)
				}
			})
		})
	}
}

// truncate keeps a failure message readable when the payload is 1200 bytes of padding.
func truncate(s string) string {
	const max = 24
	if len(s) <= max {
		return s
	}
	return s[:max] + "...(" + itoa(len(s)) + "B)"
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
