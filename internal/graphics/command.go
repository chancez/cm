// Package graphics interprets the kitty graphics protocol commands a program sends to its terminal.
//
// It exists because cm cannot treat these as opaque bytes to forward, and the reason is specific: a
// command may name a *file* rather than carry its data, and the file is consumed and unlinked by
// whichever process reads it first. Forwarding such a command means two readers racing for a
// single-use file, which is what made `kitten icat` report
// "EBADF ... No such file or directory" for two of its three capability probes inside a cm session
// while the same kitty answered them cleanly with no cm in between.
//
// The other reason is re-emission. libghostty stores what a payload decodes to and discards the
// payload, so a screen restored from its storage would have to re-encode raw pixels: measured at 90x
// the inbound size for a 1712x1294 image, 11815084 bytes against 217378. Keeping the bytes a program
// sent makes a restore a verbatim replay instead. So cm parses enough of the protocol to know an
// image's identity and to hold its payload, and no more.
//
// Deliberately not a full implementation. cm does not render, so nothing here decodes pixels, resolves
// placements, or answers a query. See docs/architecture.md on what cm presents itself as.
package graphics

import (
	"bytes"
	"strconv"
	"strings"
)

// Intro and terminators for an APC-wrapped graphics command: ESC _ G ... ESC \ or ESC _ G ... BEL.
const (
	intro = "\x1b_G"
	// st is the string terminator, ESC backslash. Programs also use BEL, which stringEnd accepts.
	st = "\x1b\\"
)

// Action is what a command asks the terminal to do, from the a= key.
type Action byte

const (
	// ActionTransmit stores an image without displaying it (a=t).
	ActionTransmit Action = 't'
	// ActionTransmitAndDisplay stores and displays in one command (a=T).
	ActionTransmitAndDisplay Action = 'T'
	// ActionQuery asks whether a transmission would work, without storing (a=q).
	//
	// The one cm has to care about beyond forwarding: icat sends three of these to discover which
	// transfer media the terminal supports, and answers to them decide whether it can send an image
	// at all.
	ActionQuery Action = 'q'
	// ActionPut displays an already-stored image (a=p).
	ActionPut Action = 'p'
	// ActionDelete removes images or placements (a=d).
	ActionDelete Action = 'd'
	// ActionAnimate controls animation frames (a=a).
	ActionAnimate Action = 'a'
	// ActionComposeFrame composes an animation frame (a=c).
	ActionComposeFrame Action = 'c'
)

// Medium is how a command's data reaches the terminal, from the t= key.
type Medium byte

const (
	// MediumDirect carries the data inline, base64 encoded in the payload (t=d). The default when a
	// command names no medium.
	MediumDirect Medium = 'd'
	// MediumFile names a regular file holding the data (t=f).
	MediumFile Medium = 'f'
	// MediumTempFile names a file the terminal should delete after reading (t=t).
	//
	// The medium behind the reported failure. Whoever reads it unlinks it, so it cannot be read twice
	// and therefore cannot be forwarded to a second reader.
	MediumTempFile Medium = 't'
	// MediumSharedMemory names a POSIX shared memory object, which the reader unlinks (t=s).
	MediumSharedMemory Medium = 's'
)

// NeedsFile reports whether this medium names something on the filesystem rather than carrying data.
//
// The property that matters for forwarding: these are single-use, so exactly one reader may consume
// one.
func (m Medium) NeedsFile() bool {
	return m == MediumFile || m == MediumTempFile || m == MediumSharedMemory
}

// Command is a parsed graphics command.
//
// Only the keys cm needs are broken out. Everything else stays in Control verbatim, so a command can
// be rebuilt without cm having to understand keys it does not use, and a protocol extension does not
// silently lose parameters.
type Command struct {
	// Action is the a= key, ActionTransmitAndDisplay when absent, matching the protocol default.
	Action Action
	// Medium is the t= key, MediumDirect when absent.
	Medium Medium
	// ImageID is the i= key. Zero when absent, which is meaningful: a program may address an image by
	// Number instead.
	ImageID uint32
	// ImageNumber is the I= key, an alternative to an id.
	ImageNumber uint32
	// PlacementID is the p= key.
	PlacementID uint32
	// Quiet is the q= key: 1 suppresses success responses, 2 suppresses all of them.
	//
	// Load-bearing for re-emission. cm re-sends stored images with q=2 so the client's terminal
	// generates no responses, which keeps re-emitted images out of the reply routing entirely.
	Quiet uint8
	// More reports the m= key, set while further chunks of one image's data are still coming.
	More bool
	// Control is the raw control section, so Encode can rebuild the command without interpreting
	// every key.
	Control string
	// Payload is the raw payload as it appeared, still base64 encoded. Empty for a command that
	// carries none.
	Payload []byte
	// Raw is the whole command including introducer and terminator, so forwarding is byte-exact.
	Raw []byte
}

// IsTransmission reports whether this command carries or names image data to store.
func (c Command) IsTransmission() bool {
	return c.Action == ActionTransmit || c.Action == ActionTransmitAndDisplay
}

// Key identifies the image a command refers to, preferring the id and falling back to the number.
//
// Both exist because the protocol lets a program pick either, and a store keyed on only one would
// miss half the traffic. The bool distinguishes "numbered zero" from "no identity at all", which a
// caller must not conflate: a command with neither cannot be recalled later and so cannot be stored.
func (c Command) Key() (id uint32, byNumber bool, ok bool) {
	if c.ImageID != 0 {
		return c.ImageID, false, true
	}
	if c.ImageNumber != 0 {
		return c.ImageNumber, true, true
	}
	return 0, false, false
}

// Parse reads one graphics command from the front of p.
//
// Reports n as the number of bytes consumed and ok only for a complete command. A zero n means p does
// not begin with a graphics introducer at all; a positive n with ok false means the introducer is
// there but the terminator has not arrived yet, which is the case a caller has to buffer rather than
// discard. That distinction is why this returns three values: a chunk from a pty splits at 1022 bytes
// on darwin regardless of the read size, so a large transmission is guaranteed to arrive in pieces
// and treating a fragment as malformed would corrupt every image over that size.
func Parse(p []byte) (cmd Command, n int, ok bool) { return ParseFrom(p, 0) }

// ParseFrom is Parse resuming its terminator search at an offset into the command's body.
//
// For a caller reassembling one command across many chunks: passing how much of the body it has already
// examined makes each byte looked at once rather than once per chunk. Zero behaves exactly like Parse.
//
// The offset is into the body, after the introducer, because that is what a caller accumulating payload
// naturally knows.
func ParseFrom(p []byte, searched int) (cmd Command, n int, ok bool) {
	if !bytes.HasPrefix(p, []byte(intro)) {
		// A partial introducer at the very end of a chunk is not a command yet, but must not be
		// reported as "not graphics" either, or the caller drops the bytes that would complete it.
		if prefixLen(p) > 0 {
			return Command{}, len(p), false
		}
		return Command{}, 0, false
	}

	body := p[len(intro):]
	end, termLen := stringEndFrom(body, searched)
	if end < 0 {
		// Incomplete: the terminator is still to come.
		return Command{}, len(p), false
	}

	full := body[:end]
	total := len(intro) + end + termLen

	// The control section runs to the first semicolon; everything after it is payload. A command with
	// no semicolon is all control, which is normal for a delete or a put.
	control, payload := full, []byte(nil)
	if i := bytes.IndexByte(full, ';'); i >= 0 {
		control, payload = full[:i], full[i+1:]
	}

	cmd = Command{
		// Defaults from the protocol, applied before parsing so an absent key means the default
		// rather than a zero value that happens to be wrong.
		Action:  ActionTransmitAndDisplay,
		Medium:  MediumDirect,
		Control: string(control),
		Raw:     p[:total],
	}
	if len(payload) > 0 {
		cmd.Payload = payload
	}

	for _, kv := range strings.Split(string(control), ",") {
		k, v, found := strings.Cut(kv, "=")
		if !found || v == "" {
			continue
		}
		switch k {
		case "a":
			cmd.Action = Action(v[0])
		case "t":
			cmd.Medium = Medium(v[0])
		case "i":
			cmd.ImageID = parseUint32(v)
		case "I":
			cmd.ImageNumber = parseUint32(v)
		case "p":
			cmd.PlacementID = parseUint32(v)
		case "q":
			cmd.Quiet = uint8(parseUint32(v))
		case "m":
			cmd.More = v == "1"
		}
	}

	return cmd, total, true
}

// parseUint32 reads a decimal value, yielding zero for anything unparseable.
//
// Zero rather than an error because every field using it treats zero as absent, and a program sending
// a malformed key should not make cm reject a command a real terminal would accept: the terminal is
// the authority on the rest of the protocol, and cm is only reading the parts it needs.
func parseUint32(s string) uint32 {
	v, err := strconv.ParseUint(s, 10, 32)
	if err != nil {
		return 0
	}
	return uint32(v)
}

// stringEnd finds the terminator of an APC string, returning its offset and length.
//
// Both terminators are accepted because programs use both: ST is what the protocol specifies and BEL
// is what several implementations send, so recognizing only one would leave half of them unterminated
// forever.
func stringEnd(p []byte) (end, termLen int) { return stringEndFrom(p, 0) }

// stringEndFrom is stringEnd resuming at an offset already known to hold no terminator.
//
// Exists so a caller scanning a command across many chunks does not re-walk the payload it has already
// examined. Both halves of that mattered when measured: without resuming, reassembling a 129 chunk image
// put 92.77% of the time in this function, and scanning twice per chunk to check-then-locate cost 62%
// on a chunk that completes a command.
func stringEndFrom(p []byte, from int) (end, termLen int) {
	if from < 0 {
		from = 0
	}
	for i := from; i < len(p); i++ {
		switch p[i] {
		case 0x07:
			return i, 1
		case 0x1b:
			if i+1 < len(p) && p[i+1] == '\\' {
				return i, 2
			}
		}
	}
	return -1, 0
}

// prefixLen reports how many trailing bytes of p could be the start of an introducer.
//
// Needed so a chunk ending in ESC or ESC _ is held rather than passed on: the bytes that complete the
// introducer are in the next chunk, and a scan that only looked for the whole thing would emit the
// fragment as ordinary output and then fail to recognize the remainder.
func prefixLen(p []byte) int {
	max := min(len(intro)-1, len(p))
	for n := max; n > 0; n-- {
		if bytes.HasSuffix(p, []byte(intro[:n])) {
			return n
		}
	}
	return 0
}

// Encode builds a graphics command from a control section and payload.
//
// Used to re-emit a stored image, which is why quiet is a parameter rather than taken from the
// original: cm re-sends with q=2 so the client's terminal produces no responses. A re-emitted image
// that generated responses would put bytes on the input path answering questions cm never asked,
// which is the failure the interception exists to remove.
func Encode(control string, payload []byte) []byte {
	out := make([]byte, 0, len(intro)+len(control)+1+len(payload)+len(st))
	out = append(out, intro...)
	out = append(out, control...)
	if len(payload) > 0 {
		out = append(out, ';')
		out = append(out, payload...)
	}
	out = append(out, st...)
	return out
}

// WithQuiet returns a control section with its q= key forced to the given level.
//
// Rewriting rather than appending, because a duplicate key is undefined by the protocol and a
// terminal may honor either occurrence.
func WithQuiet(control string, level uint8) string {
	parts := strings.Split(control, ",")
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, kv := range parts {
		if k, _, found := strings.Cut(kv, "="); found && k == "q" {
			out = append(out, "q="+strconv.Itoa(int(level)))
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, "q="+strconv.Itoa(int(level)))
	}
	return strings.Join(out, ",")
}
