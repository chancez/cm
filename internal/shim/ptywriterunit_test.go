package shim

import (
	"reflect"
	"sync"
	"testing"
	"time"
)

// pauseWindow is how long the second writer is given to cut in before the first is let go.
//
// Only the passing path pays it. A writer with nothing in its way reaches the destination in
// microseconds, so an unserialized write lands well inside this window and the assertion below sees it;
// a serialized one is still on the mutex when the window closes. Set far above the gap it needs to
// observe rather than near it, because the cost is one 50ms wait in a unit test and the alternative is a
// test that reports "serialized" when it merely lost a race.
const pauseWindow = 50 * time.Millisecond

// A second writer waits rather than cutting into a write already in progress.
//
// The end-to-end version, TestConcurrentPtyWritesDoNotInterleave, needs a real pty and caught this 3
// times in 120 runs on Linux and never on darwin. That is not standing guard, so the invariant is stated
// here as well, where the window can be held open rather than raced: the destination pauses inside a
// write, which is what os.File.Write does between the syscalls a payload larger than the tty's buffer
// takes.
//
// Deliberately not synctest, which is the repo's default for concurrency and cannot express this. Its
// documentation is explicit that locking a sync.Mutex is not durably blocking, so Wait never returns
// while the second writer is on the lock, and the test hangs instead of asserting. A real short wait is
// what is left, and it is only spent when the code is correct.
func TestPtyWriterSerializesConcurrentWrites(t *testing.T) {
	// Shaped like the pair that collided: a clipboard reply and a resize report.
	reply := []byte("\x1b]52;c;QUJDRA\x07")
	report := []byte("\x1b[48;24;80t")

	dest := &pausingWriter{
		pauseOn: string(reply),
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	w := &ptyWriter{w: dest}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		if n, err := w.Write(reply); n != len(reply) || err != nil {
			t.Errorf("writing the reply = (%d, %v), want (%d, nil)", n, err, len(reply))
		}
	}()

	// The reply is now halfway into the destination, which is where a real write sits between two
	// syscalls.
	<-dest.entered

	wg.Add(1)
	go func() {
		defer wg.Done()
		if n, err := w.Write(report); n != len(report) || err != nil {
			t.Errorf("writing the report = (%d, %v), want (%d, nil)", n, err, len(report))
		}
	}()

	// Every chance to cut in. Without an ordering point the report has nothing to block on and is
	// through the destination long before this returns.
	time.Sleep(pauseWindow)
	close(dest.release)
	wg.Wait()

	// The reply arrived in one piece and the report after it rather than inside it. Asserted on the whole
	// byte stream rather than on "the reply is contiguous", because the ordering is the invariant and a
	// containment check would pass on a stream that also held the report in the wrong place.
	got := dest.written()
	want := append(append([]byte(nil), reply...), report...)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the destination received %q\nwant %q\n"+
			"a write in progress was interrupted, which on a real pty aborts the escape sequence and "+
			"prints the rest of it as text in whatever program is running", got, want)
	}
}

// pausingWriter records what it is given and stops in the middle of one write.
//
// Two halves with a pause between, because that is the shape of the real thing: os.File.Write loops over
// write(2) for a payload the tty cannot take at once, and the gap between two of those syscalls is a
// scheduling point where another goroutine's write used to land.
type pausingWriter struct {
	mu  sync.Mutex
	got []byte

	// pauseOn is the payload to stop inside, so only the write under test pauses and the second one runs
	// straight through once it is allowed in. Matched by content rather than by counting calls, so it
	// does not depend on which write arrives first.
	pauseOn string
	entered chan struct{}
	release chan struct{}
	// paused guards against pausing twice, which would deadlock if the same payload were written again.
	paused bool
}

func (p *pausingWriter) Write(b []byte) (int, error) {
	if !p.shouldPause(b) {
		p.append(b)
		return len(b), nil
	}

	half := len(b) / 2
	p.append(b[:half])
	close(p.entered)
	<-p.release
	p.append(b[half:])
	return len(b), nil
}

// shouldPause reports whether this is the write to stop inside, and records that it has been.
func (p *pausingWriter) shouldPause(b []byte) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.paused || string(b) != p.pauseOn {
		return false
	}
	p.paused = true
	return true
}

func (p *pausingWriter) append(b []byte) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.got = append(p.got, b...)
}

// written returns a copy, so the caller can read it while a write is still in progress.
func (p *pausingWriter) written() []byte {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]byte(nil), p.got...)
}
