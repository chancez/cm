package client

import (
	"reflect"
	"testing"
	"time"
)

// The memory's whole job is to survive a dropped stream and hand the questions back once.
func TestPendingQueriesAreReturnedOnce(t *testing.T) {
	var p pendingQueries
	p.add([]byte("\x1b]11;?\x07"))
	p.add([]byte("\x1b[14t"))

	want := [][]byte{[]byte("\x1b]11;?\x07"), []byte("\x1b[14t")}
	if got := p.take(); !reflect.DeepEqual(got, want) {
		t.Errorf("take() = %q, want %q", got, want)
	}
	// Consumed, so a client that keeps reconnecting does not re-offer the same question every time and hold
	// the reply queue on every attempt.
	if got := p.take(); got != nil {
		t.Errorf("take() = %q on a second call, want nil", got)
	}
}

// A question older than queryMemory is not worth re-offering: the server would record it, hold the queue for
// its own requestTimeout, and abandon it.
func TestPendingQueriesForgetsOldOnes(t *testing.T) {
	base := time.Unix(0, 0)
	now := base
	p := pendingQueries{now: func() time.Time { return now }}

	p.add([]byte("stale"))
	now = base.Add(queryMemory + time.Second)
	p.add([]byte("fresh"))

	want := [][]byte{[]byte("fresh")}
	if got := p.take(); !reflect.DeepEqual(got, want) {
		t.Errorf("take() = %q, want only the recent one: %q", got, want)
	}
}

// The count is bounded, since a client that never answers would otherwise accumulate one entry per question for
// queryMemory. The oldest goes first, because a newer question is the one more likely still to be waiting.
func TestPendingQueriesIsBounded(t *testing.T) {
	var p pendingQueries
	for i := range maxPendingQueries + 3 {
		p.add([]byte{byte('a' + i)})
	}

	got := p.take()
	if len(got) != maxPendingQueries {
		t.Fatalf("take() returned %d entries, want at most %d", len(got), maxPendingQueries)
	}
	// The first three were dropped, so the oldest kept is the fourth added.
	if string(got[0]) != string([]byte{byte('a' + 3)}) {
		t.Errorf("oldest kept entry = %q, want the fourth added: the newest are the ones worth keeping",
			got[0])
	}
}

// An empty question is not recorded, so a caller cannot fill the memory with nothing and push out real ones.
func TestPendingQueriesIgnoresEmpty(t *testing.T) {
	var p pendingQueries
	p.add(nil)
	p.add([]byte{})
	if got := p.take(); got != nil {
		t.Errorf("take() = %q after adding only empty values, want nil", got)
	}
}
