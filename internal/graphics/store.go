package graphics

import (
	"sync"
)

// DefaultMaxBytes bounds a store's retained payloads.
//
// 32 MiB, chosen against two measurements rather than picked. The payloads kept here are what a program
// sent, still compressed, so the captured 1712x1294 screenshot occupies 217378 bytes: this holds about
// 150 of those. The comparison that matters is libghostty's own storage, which keeps *decoded* pixels
// and defaults to 10000000 bytes, or 1.1 images of that size. So a compressed store an order of
// magnitude larger still costs less memory per image and holds two orders of magnitude more of them.
//
// Bounded at all because a session lives for days and a program may transmit an image per frame. The
// eviction below drops the least recently used, which for images is the right end: a screen being
// restored needs what is currently placed, and what is currently placed is what was touched most
// recently.
const DefaultMaxBytes = 32 << 20

// Store holds the graphics payloads a session has transmitted, so they can be re-sent verbatim.
//
// Separate from libghostty's image storage, and the split is deliberate. libghostty holds decoded
// pixels because it renders; this holds the original bytes because cm re-transmits. Reconstructing a
// transmission from libghostty's copy would mean re-encoding, measured at 90x the inbound size, so the
// two stores answer different questions and neither replaces the other.
//
// Safe for concurrent use. The output pump writes to it while an attaching client reads, which is the
// same pattern as the session's other state and the reason this carries its own mutex rather than
// borrowing the session's.
type Store struct {
	mu       sync.Mutex
	max      int
	bytes    int
	images   map[key]*entry
	sequence uint64
	// loading is the key of the transmission whose chunks are still arriving, and whether there is one.
	//
	// Required because a continuation chunk does not say which image it belongs to. kitty's own client
	// rebuilds each chunk after the first from the a= and q= keys alone
	// (tools/tui/graphics/command.go), so an i= appears once, on the first chunk, and everything after it
	// is anonymous payload. Keying each chunk by its own identity therefore dropped every continuation and
	// left the image permanently incomplete, so it was never re-sent to an attaching client.
	loading    key
	hasLoading bool
}

// key identifies an image by the addressing scheme the program used.
//
// Both id and number are in the key rather than resolved to one, because the protocol lets a program
// use either and they are separate namespaces: image number 4 and image id 4 are different images, so
// collapsing them would have one transmission overwrite the other.
type key struct {
	id       uint32
	byNumber bool
}

// entry is one image's retained transmission.
type entry struct {
	// control is the control section of the command that transmitted it, less any chunking, so a
	// re-emission can rebuild an equivalent command.
	control string
	// payload is the accumulated base64 payload as the program sent it.
	payload []byte
	// used orders entries for eviction, newest highest.
	used uint64
	// complete reports whether the final chunk has arrived. An incomplete image is retained so the
	// remaining chunks can be appended, but must not be re-emitted: half an image is worse than none,
	// since a terminal would draw a partial or corrupt picture rather than nothing.
	complete bool
}

// NewStore returns a store bounded at max bytes, or DefaultMaxBytes when max is not positive.
func NewStore(max int) *Store {
	if max <= 0 {
		max = DefaultMaxBytes
	}
	return &Store{max: max, images: make(map[key]*entry)}
}

// Add records a transmission command, accumulating chunks until the image is complete.
//
// Chunked transmissions are the normal case rather than an edge one: a pty read caps at 1022 bytes on
// darwin, and the captured icat run sent one image as `m=1` chunks, so an image arrives as a first
// command carrying control keys and payload followed by continuations carrying payload alone. The
// control section is taken from the first chunk, since later ones legitimately carry only `m=` and
// `q=`.
//
// A transmission naming no image is still ignored, and that is a decision rather than an oversight. icat's
// default transfer names none at all: the captured commands are `a=T,q=2,f=24,o=z,m=1,s=800,v=503`, then
// `a=T,q=2,m=1`, then `a=T,q=2`, so the terminal assigns the id. cm cannot re-emit one usefully, because a
// re-emission would be given a fresh id by whatever terminal received it and no later `a=p` from the
// program would resolve against it. Retaining them was measured to be actively harmful: the icat image
// above is 1207200 bytes of RGB, which cm inlines as 1609600 base64 characters, and re-sending it on every
// attach made the restore blob 4.3 MB. That wedged the session, since a client re-attaching for any reason
// received megabytes it could not draw, and it drew nothing because a restored screen carries no placement.
func (s *Store) Add(cmd Command) {
	if !cmd.IsTransmission() {
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	k, continuing, ok := s.keyForLocked(cmd)
	if !ok {
		return
	}
	// Stored without m=, because the section kept here describes the *image* while m= describes how the
	// program happened to split it. See DropChunking: keeping it re-emitted a first chunk's m=1 with no
	// chunks behind it, so the image never completed at the far end.
	control := DropChunking(cmd.Control)
	e := s.images[k]
	if e == nil {
		e = &entry{control: control}
		s.images[k] = e
	} else if e.complete && !continuing {
		// A new transmission for an id that already had one replaces it. The protocol allows reusing
		// an id, and keeping the old bytes would have a restore draw the previous image.
		s.bytes -= len(e.payload)
		*e = entry{control: control}
	}

	e.payload = append(e.payload, cmd.Payload...)
	s.bytes += len(cmd.Payload)
	s.sequence++
	e.used = s.sequence
	// m=1 says more chunks follow, so anything else completes the image, including its absence.
	e.complete = !cmd.More
	// Remembered so the chunks behind it, which carry no identity of their own, reach this same entry.
	s.loading, s.hasLoading = k, cmd.More

	s.evictLocked()
}

// keyForLocked decides which entry a transmission belongs to, and whether it continues one.
//
// Three cases, in the order they have to be tried. A command naming an image is that image, which covers
// the first chunk of a transmission and any unchunked one. A command naming none while a transmission is
// still arriving is the next chunk of it, and that is the case keying by identity alone got wrong: kitty's
// client repeats only a= and q= after the first chunk, so every continuation looked unidentifiable and was
// dropped. A command naming none with nothing arriving is one cm cannot recall, and reports ok=false.
//
// A new transmission and a continuation chunk are indistinguishable when neither names an image, so this
// treats one as the other. That is what a terminal does too: kitty tracks the image it is loading and
// appends to it, and the protocol does not interleave transmissions.
//
// Callers must hold mu.
func (s *Store) keyForLocked(cmd Command) (k key, continuing, ok bool) {
	if id, byNumber, named := cmd.Key(); named {
		k = key{id: id, byNumber: byNumber}
		return k, s.hasLoading && s.loading == k, true
	}
	if s.hasLoading {
		return s.loading, true, true
	}
	return key{}, false, false
}

// Touch marks an image as recently used without changing it.
//
// Called when a command displays an already-stored image, so eviction sees a placement as use. Without
// this an image transmitted once and displayed repeatedly would age out while still on screen, and the
// restore would be missing exactly the picture the user is looking at.
func (s *Store) Touch(id uint32, byNumber bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if e := s.images[key{id: id, byNumber: byNumber}]; e != nil {
		s.sequence++
		e.used = s.sequence
	}
}

// Delete forgets an image, for a program that deletes one explicitly.
func (s *Store) Delete(id uint32, byNumber bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	k := key{id: id, byNumber: byNumber}
	if e := s.images[k]; e != nil {
		s.bytes -= len(e.payload)
		delete(s.images, k)
	}
	// A transmission cannot continue into an entry that is gone: its chunks would rebuild the image under
	// the key the program just deleted, with a control section taken from a continuation chunk and so
	// carrying no geometry at all.
	if s.hasLoading && s.loading == k {
		s.hasLoading = false
	}
}

// Reset forgets everything, for a program that clears all images.
func (s *Store) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.images = make(map[key]*entry)
	s.bytes = 0
	s.hasLoading = false
}

// Retransmission is a command that re-sends one stored image.
type Retransmission struct {
	// ID and ByNumber identify the image, so a caller can correlate it with a placement.
	ID       uint32
	ByNumber bool
	// Bytes is the command to write, already quiet-suppressed.
	Bytes []byte
}

// Retransmissions builds the commands that re-send every complete stored image.
//
// Every stored image rather than only the placed ones: a placement can refer to an image transmitted
// without being displayed, and a program may place it later, so sending only what is on screen would
// break the next `a=p`. The order is oldest-used first, so a terminal receives images in the order
// they were transmitted.
//
// Each command is forced to q=2, which suppresses both success and error responses. That is the whole
// reason re-emission is safe: an image cm sends generates no reply, so nothing arrives on the input
// path answering a question cm never asked. Forwarding a program's own transmission unchanged is what
// produced the reported echo, and this is deliberately not that.
//
// Each is also forced to a=t, store without displaying, and that is the correctness half rather than a
// tidy-up. A stored command is usually a=T, transmit *and display at the cursor*, so re-emitting it
// verbatim drew the image wherever the cursor happened to be at that instant in the restore. Measured in
// a real kitty: a client attaching while a full-screen program was running received
// "ESC[2J", the image as a=T, then "ESC[?1049l ESC[2J", then "ESC[?1049h" and the program's screen, so
// the image was placed on the main screen and then erased by the clear behind it. Land the same bytes a
// moment later and it is drawn *on top of* the program instead, which is the reported image sitting over
// fzf. Where an image belongs is a placement, and a placement is restored separately from the model.
//
// Chunked back out at kitty's own limit rather than emitted as one command, because a retained payload is
// a whole image and a single command that size is one a terminal may discard. See EncodeChunks.
func (s *Store) Retransmissions() []Retransmission {
	s.mu.Lock()
	defer s.mu.Unlock()

	ordered := make([]struct {
		k key
		e *entry
	}, 0, len(s.images))
	for k, e := range s.images {
		if !e.complete {
			continue
		}
		ordered = append(ordered, struct {
			k key
			e *entry
		}{k, e})
	}
	// Insertion sort by use order. The count is small, bounded by the store's byte limit divided by an
	// image's size, and this runs on attach rather than per chunk.
	for i := 1; i < len(ordered); i++ {
		for j := i; j > 0 && ordered[j-1].e.used > ordered[j].e.used; j-- {
			ordered[j-1], ordered[j] = ordered[j], ordered[j-1]
		}
	}

	out := make([]Retransmission, 0, len(ordered))
	for _, o := range ordered {
		out = append(out, Retransmission{
			ID:       o.k.id,
			ByNumber: o.k.byNumber,
			Bytes:    EncodeChunks(WithQuiet(withKey(o.e.control, "a", "t"), 2), o.e.payload),
		})
	}
	return out
}

// Len reports how many images are retained, for diagnostics and tests.
func (s *Store) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.images)
}

// Bytes reports the retained payload size, for diagnostics and tests.
func (s *Store) Bytes() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bytes
}

// evictLocked drops least-recently-used entries until the store is within its limit.
//
// An incomplete entry is evictable like any other. It would be tempting to protect one, since its
// remaining chunks are still arriving, but a transmission larger than the whole limit would then never
// terminate and would wedge the store: the bound has to hold whatever the program does. The cost of
// evicting one is a missing image on a later restore, which is the same cost as evicting a complete
// one.
//
// Callers must hold mu.
func (s *Store) evictLocked() {
	for s.bytes > s.max && len(s.images) > 0 {
		var oldest key
		var oldestUsed uint64
		first := true
		for k, e := range s.images {
			if first || e.used < oldestUsed {
				oldest, oldestUsed, first = k, e.used, false
			}
		}
		s.bytes -= len(s.images[oldest].payload)
		delete(s.images, oldest)
	}
}
