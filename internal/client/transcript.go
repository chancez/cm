//go:build cm_testhooks

package client

import (
	"encoding/json"
	"os"
	"sync"
)

// Transcript records every write a client makes to its terminal, tagged with where it came from, so a
// test can check what the terminal actually received rather than what cm believes it sent.
//
// This is the instrumentation that found the window-title bug, made permanent. That bug took three
// rounds to locate because every capture taken inside cm missed a writer that bypassed cm's own
// abstraction, and an incomplete capture that replays clean reads as proof the bytes are fine. What
// finally settled it was the terminal's own record, from `kitty --dump-bytes`. This is the same record
// without needing kitty: one entry per write, in order, with its kind.
//
// Behind cm_testhooks so a released binary does not contain it. An env var a shipped binary honors is
// one a stale export can use to make it lie, and this one would write a session's output to a file.
//
// The point is not this one bug. Any test that drives a client can now assert the invariant, which
// means the e2e suite checks it on every run without any of those tests knowing about it. See
// ansi.ValidateTranscript.
type Transcript struct {
	mu      sync.Mutex
	f       *os.File
	Entries []TranscriptEntry
}

// TranscriptEntry is one write to the terminal.
type TranscriptEntry struct {
	// Kind is "session" for the program's own bytes and "inject" for bytes cm generated: a title, a
	// proxied query, the outage notice, the clear before a restore.
	Kind string `json:"kind"`
	Data []byte `json:"data"`
}

// newTranscript returns a recorder when CM_TESTHOOK_TRANSCRIPT names a file, and nil otherwise.
//
// Nil rather than a no-op recorder so the check at each write is a nil comparison.
func newTranscript() *Transcript {
	path := os.Getenv("CM_TESTHOOK_TRANSCRIPT")
	if path == "" {
		return nil
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		// A recorder that cannot open its file records nothing rather than failing the attachment: this
		// is a test hook, and breaking a session because of it would be worse than the missing data.
		return nil
	}
	return &Transcript{f: f}
}

// record appends one write.
//
// Written as it happens rather than buffered to the end, because the failures this exists for include a
// client that never gets to the end.
func (tr *Transcript) record(kind string, p []byte) {
	if tr == nil || len(p) == 0 {
		return
	}
	tr.mu.Lock()
	defer tr.mu.Unlock()
	e := TranscriptEntry{Kind: kind, Data: append([]byte(nil), p...)}
	tr.Entries = append(tr.Entries, e)
	if tr.f != nil {
		// One JSON object per line, so a reader can consume a truncated file: a test that kills the
		// client mid-write still gets everything up to that point.
		if b, err := json.Marshal(e); err == nil {
			tr.f.Write(append(b, '\n'))
		}
	}
}
