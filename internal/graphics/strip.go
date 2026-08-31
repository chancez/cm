package graphics

// Stripper removes graphics commands from a stream of session output.
//
// For a client whose terminal cannot draw images. cm withholds the images in a *restore* from such a client,
// which fixed a screen of base64 on a mobile ssh client, but the session's live output goes to every attached
// client alike: with a kitty and a phone both attached, an `icat` run in the kitty drew there and printed its
// payload as text on the phone. A gate on the restore alone only covers images that were already on screen.
//
// Stripping rather than repainting, and stripping in the server rather than in the client, because the bytes
// are the cost: an image is megabytes, and the link that cannot draw it is usually the slow one. Sending it to
// be discarded on arrival would delay the text behind it, which is the same symptom in a different disguise.
//
// Stateful for the reason Scanner is: a pty read caps at 1022 bytes on darwin, so a transmission always
// arrives in pieces, and a stripper that forgot between chunks would pass the tail of every image through as
// text. Not safe for concurrent use; one per attachment, used only by that attachment's output loop.
type Stripper struct {
	sc Scanner
	// out is the rebuilt chunk, reused so a steady stream of images does not allocate per chunk.
	out []byte
}

// Strip returns the chunk with every complete graphics command removed.
//
// The result aliases the stripper's own buffer, so a caller must use it before calling Strip again. Ordinary
// output is kept in place and in order, which is what makes this safe to do to a screen: a graphics command
// paints no text and moves no cursor, so what is left describes the same screen minus the pictures.
//
// An incomplete command at the end of a chunk is held, exactly as Scanner holds it, and Pending reports that.
// A caller that stops stripping while something is held would send the remainder of a half-removed command,
// and a bare `...base64...ESC \` is text on any terminal.
func (s *Stripper) Strip(p []byte) []byte {
	segs := s.sc.Scan(p)
	if segs == nil {
		// Nothing graphics-shaped and nothing held: forward the input untouched, which is the overwhelmingly
		// common case and the one that must not allocate.
		return p
	}
	s.out = s.out[:0]
	for _, seg := range segs {
		if !seg.Graphics {
			s.out = append(s.out, seg.Data...)
		}
	}
	return s.out
}

// Pending reports whether part of a command is held, waiting for the rest to arrive.
func (s *Stripper) Pending() bool { return s.sc.Pending() > 0 }

// Reset discards anything held.
//
// For a gap in the session's output, where the bytes that would have completed a held command are the ones
// that were lost. Holding them across a gap would strip the front of whatever arrives next instead.
func (s *Stripper) Reset() { s.sc.Reset() }
