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
	// Width, Height, and Format are the s=, v=, and f= keys: the image's geometry.
	//
	// Needed because a terminal derives the expected byte count from these rather than from S= for a raw
	// pixel format. kitty computes s*v*(f/8) and rejects a payload larger than that plus a small margin,
	// so an inlined transfer whose bytes came from a page-rounded container is refused with
	// "EFBIG: Too much data". Zero when absent.
	Width, Height uint32
	Format        uint32
	// DataSize is the S= key: how many bytes of the named container hold the image.
	//
	// Load-bearing for shared memory rather than informational. An shm object is rounded up to a page,
	// so a 3 byte payload arrives in a 4096 byte object, and reading the whole container hands the
	// terminal 4093 bytes of zero padding it never sent. icat states the real length here for exactly
	// that reason, and its capability probes are visible doing it: "S=3", "S=87", "S=18".
	//
	// Zero when absent, meaning "the whole container".
	DataSize uint32
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

// ExpectedBytes reports how many bytes of image data the geometry implies, or zero when it cannot say.
//
// This is what a terminal actually checks a raw payload against. kitty computes s*v*(f/8) for RGB and
// RGBA and allocates that plus ten bytes, so a payload beyond it is rejected outright rather than
// truncated. A container read whole gives more than this whenever the container is larger than the
// image, which is always true of shared memory: an object is rounded up to a page, so three bytes of
// image arrive inside 4096.
//
// Zero for PNG and for anything without geometry, where the payload's own length is the only truth and
// S= is a hint about the container rather than the image.
func (c Command) ExpectedBytes() int {
	switch c.Format {
	case 24:
		// RGB, three bytes per pixel.
	case 32:
		// RGBA, four bytes per pixel.
	default:
		// 100 is PNG, whose decoded size is not derivable from the geometry, and an absent f= defaults
		// to RGBA in the protocol but is not worth guessing for a bound.
		return 0
	}
	if c.Width == 0 || c.Height == 0 {
		return 0
	}
	return int(c.Width) * int(c.Height) * int(c.Format/8)
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
		case "s":
			cmd.Width = parseUint32(v)
		case "v":
			cmd.Height = parseUint32(v)
		case "f":
			cmd.Format = parseUint32(v)
		case "S":
			cmd.DataSize = parseUint32(v)
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

// MaxCommandPayload bounds the base64 payload cm puts in one command.
//
// 128 KiB, which is kitty's own chunk size in `tools/tui/graphics/command.go`, and it is measured against
// kitty's parser rather than picked. MAX_ESCAPE_CODE_LENGTH in `kitty/vt-parser.c` is BUF_SZ/4, so 256
// KiB, and kitty discards an escape code longer than that the moment a parser pass ends with the
// terminator still missing: "APC escape code too long (%zu bytes), ignoring it". A complete code already
// in the buffer is accepted however long it is, which is what makes an oversized command *sometimes*
// work. It depends on whether the whole thing lands before kitty's input_delay expires, so the same
// command with the same image displays or vanishes according to write timing.
//
// cm hit this by inlining a whole file into one command: the reported 565398 byte image is 753864 base64
// characters, 2.9x the limit. kitty's own clients never exceed 128 KiB per chunk for exactly this reason,
// so matching them means cm sends what a terminal already has to handle.
const MaxCommandPayload = 128 << 10

// EncodeChunks builds one or more commands carrying a payload, splitting it the way kitty's clients do.
//
// A single command when the payload fits, which is the common case and is byte-identical to Encode.
// Beyond that the payload is split at MaxCommandPayload, the first command carrying the full control
// section and each later one carrying only the keys kitty's own client repeats: a=, q=, and the m= that
// says whether more follows. Every chunk boundary is a multiple of four, since MaxCommandPayload is, so
// a terminal decoding incrementally never sees a partial base64 quantum: padding can only appear on the
// last chunk.
func EncodeChunks(control string, payload []byte) []byte {
	if len(payload) <= MaxCommandPayload {
		return Encode(control, payload)
	}
	// Only a= and q= are repeated, matching kitty's client, which rebuilds each later chunk from those two
	// alone. Repeating the geometry or the medium instead would restate keys the terminal has already
	// applied to the image it is loading.
	continuation := keepKeys(control, "a", "q")
	var out []byte
	first := true
	for len(payload) > 0 {
		n := min(MaxCommandPayload, len(payload))
		chunk := payload[:n]
		payload = payload[n:]

		section := continuation
		if first {
			section = control
			first = false
		}
		// Stated explicitly on the last chunk too, m=0, because that is what terminates the transmission:
		// leaving m off entirely would also do it, and kitty's client says m=0, so this says m=0.
		out = append(out, Encode(withKey(section, "m", boolKey(len(payload) > 0)), chunk)...)
	}
	return out
}

// boolKey renders a protocol flag, which is "1" or "0" rather than a word.
func boolKey(v bool) string {
	if v {
		return "1"
	}
	return "0"
}

// WithQuiet returns a control section with its q= key forced to the given level.
//
// Rewriting rather than appending, because a duplicate key is undefined by the protocol and a
// terminal may honor either occurrence.
func WithQuiet(control string, level uint8) string {
	return withKey(control, "q", strconv.Itoa(int(level)))
}

// withKey sets a key in a control section, replacing it in place or appending it.
//
// In place rather than always appending, because a duplicate key is undefined by the protocol and a
// terminal may honor either occurrence.
func withKey(control, key, value string) string {
	parts := strings.Split(control, ",")
	out := make([]string, 0, len(parts)+1)
	replaced := false
	for _, kv := range parts {
		if k, _, found := strings.Cut(kv, "="); found && k == key {
			out = append(out, key+"="+value)
			replaced = true
			continue
		}
		out = append(out, kv)
	}
	if !replaced {
		out = append(out, key+"="+value)
	}
	return strings.Join(out, ",")
}

// keepKeys reduces a control section to the named keys, preserving their order.
func keepKeys(control string, keys ...string) string {
	keep := make(map[string]bool, len(keys))
	for _, k := range keys {
		keep[k] = true
	}
	parts := strings.Split(control, ",")
	out := make([]string, 0, len(keys))
	for _, kv := range parts {
		if k, _, found := strings.Cut(kv, "="); found && keep[k] {
			out = append(out, kv)
		}
	}
	return strings.Join(out, ",")
}

// FirstUnclaimedID is where cm starts numbering images a program left unnamed.
//
// High on purpose. A program that names its own images picks small ids, icat uses the low hundreds, and
// kitty's own id space is 32 bit, so starting here keeps cm's numbering clear of anything a program is
// likely to choose. A collision would not be a crash: it would silently replace a program's image with
// cm's, which is far worse than a number that looks odd in a log.
const FirstUnclaimedID = 1 << 30

// WithImageID returns the command with an i= key, for a transmission that named no image.
//
// This is what makes an unnamed image restorable, and the reason it has to exist is icat: its transfers
// carry no i= at all, so the terminal assigns one. That leaves cm unable to say anything about the image
// later, because the id it would have to use in a=p is one only the terminal knows, and a second terminal
// attaching later would assign a different one. So cm names the image before anyone else sees it, and then
// every terminal, cm's model included, agrees on which image a placement refers to.
//
// Safe for exactly the commands it is used on, and no others. A transmission that names nothing has no
// handle the program can refer to either, since a display-immediately command needs none and a q=2 command
// is not even told the id, so adding one takes nothing away. A command that already names an image or a
// number is returned unchanged.
func WithImageID(cmd Command, id uint32) Command {
	if _, _, named := cmd.Key(); named {
		return cmd
	}
	out := cmd
	out.ImageID = id
	out.Control = withKey(cmd.Control, "i", strconv.FormatUint(uint64(id), 10))
	out.Raw = EncodeChunks(out.Control, out.Payload)
	return out
}
