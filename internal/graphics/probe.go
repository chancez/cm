package graphics

import (
	"bytes"
	"strconv"
)

// ProbeImageID names the image in cm's own capability probe.
//
// One below FirstUnclaimedID, so the three id spaces stay adjacent and visibly disjoint: a program's own
// ids, cm's probe, and the ids cm assigns to images a program left unnamed. A fixed low number like kitty's
// documented 31 would sit in the range a program picks from, and an `a=q` answer is matched by id, so a
// collision would read a program's response as cm's.
const ProbeImageID = FirstUnclaimedID - 1

// probeAnswer is what a terminal that can draw images replies with, less the id.
const probeAnswer = "OK"

// ProbeCommands asks a terminal whether it can draw kitty graphics, and asks a second question it must
// answer either way.
//
// This is the handshake the protocol already defines for a *sender*, and cm is one: it re-transmits stored
// images to an attaching client, so it owes the same question `kitten icat` asks before sending anything.
// Skipping it is what put a screen of base64 on a mobile ssh client, which had never said it could draw an
// image and was sent one anyway.
//
// The pairing with primary DA is what makes this cheap rather than a timeout. Every terminal answers
// `ESC [ c`, and a terminal that cannot draw images says nothing at all to the graphics query, so the DA
// reply arriving alone *is* the negative answer. Waiting for a silence to elapse would otherwise cost every
// attach the length of the timeout.
//
// a=q rather than a transmission: the terminal answers and stores nothing, so this leaves no image behind
// to be evicted or restored. The payload is one transparent pixel because the geometry has to describe
// something, and s=1,v=1,f=24 wants exactly three bytes.
func ProbeCommands() []byte {
	control := "i=" + strconv.FormatUint(uint64(ProbeImageID), 10) + ",s=1,v=1,a=q,t=d,f=24"
	out := Encode(control, []byte("AAAA"))
	// Primary DA, immediately after, so the pair is one write and the answers cannot be separated by
	// whatever else the terminal is doing.
	return append(out, "\x1b[c"...)
}

// IsProbeAnswer reports whether a command is this probe's answer, and whether it says yes.
//
// Both halves matter. A terminal that understands the protocol but refuses the image answers with an error
// rather than OK, which is a no, and either way the reply is cm's to consume rather than the program's to
// read: it answers a question the program never asked.
func IsProbeAnswer(cmd Command) (answer, ok bool) {
	id, byNumber, named := cmd.Key()
	if !named || byNumber || id != ProbeImageID {
		return false, false
	}
	return true, bytes.Equal(cmd.Payload, []byte(probeAnswer))
}

// DeviceAttributesEnd reports how many bytes of a primary DA reply are at the front of p, or zero.
//
// Needed because the probe pairs its question with one, so the reply has to be recognised to be consumed:
// left in the stream it reaches the program as input, and `?62;c` typed into a shell is exactly the class of
// corruption cm exists to prevent.
//
// The shape is CSI ? ... c, and the parameters are whatever the terminal claims to be, so this matches the
// frame rather than the contents.
func DeviceAttributesEnd(p []byte) int {
	const intro = "\x1b[?"
	if !bytes.HasPrefix(p, []byte(intro)) {
		return 0
	}
	if i := bytes.IndexByte(p[len(intro):], 'c'); i >= 0 {
		return len(intro) + i + 1
	}
	return 0
}
