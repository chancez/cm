//go:build !cm_testhooks

package client

// Transcript is not built into a released binary. See transcript.go for what it is and why it is
// behind a build tag.
//
// The nil-returning constructor and the nil-receiver method are what let screen call this
// unconditionally: the check at each write is a nil comparison, and the type carries no fields, so a
// shipped binary contains neither the recorder nor a flag that could turn one on.
type Transcript struct{}

func newTranscript() *Transcript { return nil }

func (tr *Transcript) record(string, []byte) {}
