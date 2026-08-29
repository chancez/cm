package seqlog

import (
	"context"
	"errors"
	"github.com/chancez/cm/internal/seq"
	"runtime"
	"strings"
	"sync"
	"testing"
	"testing/synctest"
	"time"
)

// drain reads chunks until the reader would block, returning the concatenated bytes and
// whether any chunk was flagged as a gap.
func drain(t *testing.T, r *Reader[seq.Log]) (string, bool) {
	t.Helper()
	var sb strings.Builder
	gap := false
	for {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		var (
			c   Chunk[seq.Log]
			err error
		)
		go func() {
			c, err = r.Next(ctx)
			close(done)
		}()
		// Let Next either return immediately or park, then stop it.
		synctest.Wait()
		select {
		case <-done:
		default:
			cancel()
			<-done
		}
		cancel()
		if err != nil {
			return sb.String(), gap
		}
		sb.Write(c.Data)
		if c.Gap {
			gap = true
		}
	}
}

func TestLogAppendAndRead(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](1024)
		r := l.Subscribe(0)
		defer r.Close()

		l.Append([]byte("hello "))
		l.Append([]byte("world"))

		got, gap := drain(t, r)
		if got != "hello world" || gap {
			t.Errorf("drain() = (%q, %v), want (%q, false)", got, gap, "hello world")
		}
	})
}

// Bounds is how the server learns where to resume, so its arithmetic is worth asserting
// directly rather than only through reads.
func TestLogBounds(t *testing.T) {
	l := New[seq.Log](10)

	if oldest, next := l.Bounds(); oldest != 0 || next != 0 {
		t.Errorf("empty Bounds() = (%d, %d), want (0, 0)", oldest, next)
	}

	l.Append([]byte("abcde"))
	if oldest, next := l.Bounds(); oldest != 0 || next != 5 {
		t.Errorf("Bounds() = (%d, %d), want (0, 5)", oldest, next)
	}

	// Overflow by 3: the window slides, and both bounds move together.
	l.Append([]byte("fghijkl"))
	if oldest, next := l.Bounds(); oldest != 2 || next != 12 {
		t.Errorf("after overflow Bounds() = (%d, %d), want (2, 12)", oldest, next)
	}
}

func TestLogDropsOldestWhenFull(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](5)
		l.Append([]byte("abcdefgh"))

		// Only the last 5 bytes survive, and a reader starting from 0 is told its view
		// is discontinuous.
		r := l.Subscribe(0)
		defer r.Close()
		got, gap := drain(t, r)
		if got != "defgh" || !gap {
			t.Errorf("drain() = (%q, %v), want (%q, true)", got, gap, "defgh")
		}
	})
}

// A single write larger than the buffer keeps its tail. Dropping it entirely would throw
// away the most recent output, which is exactly what a reattaching client needs.
func TestLogAppendLargerThanBuffer(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](4)
		l.Append([]byte("0123456789"))

		if oldest, next := l.Bounds(); oldest != 6 || next != 10 {
			t.Errorf("Bounds() = (%d, %d), want (6, 10)", oldest, next)
		}

		r := l.Subscribe(6)
		defer r.Close()
		got, gap := drain(t, r)
		if got != "6789" || gap {
			t.Errorf("drain() = (%q, %v), want (%q, false)", got, gap, "6789")
		}
	})
}

// The property the whole shim exists for: output continues while nobody is subscribed,
// and a returning subscriber resumes exactly where it left off with no loss and no
// duplication.
func TestResumeAfterSubscriberGoesAway(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](1024)

		r1 := l.Subscribe(0)
		l.Append([]byte("before"))
		first, _ := drain(t, r1)
		if first != "before" {
			t.Fatalf("first drain = %q, want %q", first, "before")
		}
		_, resumeFrom := l.Bounds()
		r1.Close()

		// Server is gone. Output keeps flowing.
		l.Append([]byte("-during-"))
		l.Append([]byte("restart"))

		r2 := l.Subscribe(resumeFrom)
		defer r2.Close()
		got, gap := drain(t, r2)
		if got != "-during-restart" || gap {
			t.Errorf("resumed drain = (%q, %v), want (%q, false)", got, gap, "-during-restart")
		}
	})
}

// If output overruns the buffer while nobody is reading, the resume point is gone. The
// subscriber must be told, since replaying from a later point cannot reconstruct state
// that depended on the missing bytes.
func TestResumeReportsGapWhenOutrun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](8)
		l.Append([]byte("12345678"))
		_, resumeFrom := l.Bounds() // 8

		l.Append([]byte("abcdefghij")) // pushes past the resume point

		r := l.Subscribe(resumeFrom)
		defer r.Close()
		c, err := r.Next(context.Background())
		if err != nil {
			t.Fatalf("Next() error = %v", err)
		}
		if !c.Gap {
			t.Errorf("Next() Gap = false, want true: resume point %d was dropped", resumeFrom)
		}
		if c.Seq != 10 {
			t.Errorf("Next() Seq = %d, want 10 (oldest retained)", c.Seq)
		}
	})
}

func TestSubscribeBeyondEndClampsToPresent(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](64)
		l.Append([]byte("abc"))

		// Ahead of the log: served from the present rather than rejected, and flagged as a gap.
		//
		// The flag is the part that changed, and it changed because of a bug. This branch was written
		// for a log that was reset behind a subscriber, where continuing from the present loses nothing
		// and a gap would be a lie. But it also catches a position counted in a *different numbering
		// space*, where output really is being skipped, and from inside the log the two are
		// indistinguishable. A client resuming across a server restart hit exactly that: its position
		// was in post-rewrite bytes while the new log began at the shim's count, so it was clamped
		// forward past bytes it never received and an escape sequence lost its leading ESC.
		//
		// So the conservative answer is to flag both. A spurious gap costs a resynchronize; a missing
		// one costs silent corruption that presents as a terminal bug.
		r := l.Subscribe(999)
		defer r.Close()
		l.Append([]byte("xyz"))

		got, gap := drain(t, r)
		if got != "xyz" || !gap {
			t.Errorf("drain() = (%q, %v), want (%q, true)", got, gap, "xyz")
		}
	})
}

// Next must block rather than spin or return empty chunks, and must wake on append.
func TestNextBlocksUntilAppend(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](64)
		r := l.Subscribe(0)
		defer r.Close()

		type result struct {
			c   Chunk[seq.Log]
			err error
		}
		done := make(chan result, 1)
		go func() {
			c, err := r.Next(context.Background())
			done <- result{c, err}
		}()

		// With no data, Next must be parked.
		synctest.Wait()
		select {
		case got := <-done:
			t.Fatalf("Next() returned %+v before any append", got)
		default:
		}

		l.Append([]byte("woke"))
		got := <-done
		if got.err != nil {
			t.Fatalf("Next() error = %v", got.err)
		}
		if string(got.c.Data) != "woke" {
			t.Errorf("Next() Data = %q, want %q", got.c.Data, "woke")
		}
	})
}

// Closing drains remaining output before reporting closure, so a client attaching just
// after the shell exits still sees its final output.
func TestCloseDrainsThenReportsClosed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](64)
		l.Append([]byte("last words"))
		l.Close()

		r := l.Subscribe(0)
		defer r.Close()

		c, err := r.Next(context.Background())
		if err != nil {
			t.Fatalf("Next() error = %v, want the buffered output first", err)
		}
		if string(c.Data) != "last words" {
			t.Errorf("Next() Data = %q, want %q", c.Data, "last words")
		}
		if _, err := r.Next(context.Background()); !errors.Is(err, ErrClosed) {
			t.Errorf("second Next() error = %v, want ErrClosed", err)
		}
	})
}

func TestCloseWakesBlockedReader(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](64)
		r := l.Subscribe(0)
		defer r.Close()

		errc := make(chan error, 1)
		go func() {
			_, err := r.Next(context.Background())
			errc <- err
		}()

		synctest.Wait()
		l.Close()

		if err := <-errc; !errors.Is(err, ErrClosed) {
			t.Errorf("Next() error = %v, want ErrClosed", err)
		}
	})
}

func TestNextRespectsContextCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](64)
		r := l.Subscribe(0)
		defer r.Close()

		ctx, cancel := context.WithCancel(context.Background())
		errc := make(chan error, 1)
		go func() {
			_, err := r.Next(ctx)
			errc <- err
		}()

		synctest.Wait()
		cancel()

		if err := <-errc; !errors.Is(err, context.Canceled) {
			t.Errorf("Next() error = %v, want context.Canceled", err)
		}
	})
}

// Multiple clients share one session, so every subscriber must see the full stream
// independently.
func TestMultipleSubscribersEachSeeEverything(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		l := New[seq.Log](1024)
		r1 := l.Subscribe(0)
		defer r1.Close()
		r2 := l.Subscribe(0)
		defer r2.Close()

		l.Append([]byte("shared output"))

		for i, r := range []*Reader[seq.Log]{r1, r2} {
			got, gap := drain(t, r)
			if got != "shared output" || gap {
				t.Errorf("subscriber %d drain = (%q, %v), want (%q, false)", i, got, gap, "shared output")
			}
		}
	})
}

// The window advancing must not let the backing array grow without bound, or the log
// would retain max bytes while occupying far more memory over a long session.
func TestBackingArrayStaysBounded(t *testing.T) {
	const max = 64
	l := New[seq.Log](max)
	for range 1000 {
		l.Append([]byte("0123456789"))
	}

	l.mu.Lock()
	gotLen, gotCap := len(l.buf), cap(l.buf)
	l.mu.Unlock()

	if gotLen != max {
		t.Errorf("len(buf) = %d, want %d", gotLen, max)
	}
	// Allow slack for append's growth strategy, but not unbounded accumulation.
	if gotCap > 4*max {
		t.Errorf("cap(buf) = %d, want <= %d after 10000 bytes through a %d-byte log",
			gotCap, 4*max, max)
	}
}

func TestAppendAfterCloseIsIgnored(t *testing.T) {
	l := New[seq.Log](64)
	l.Append([]byte("abc"))
	l.Close()
	l.Append([]byte("ignored"))

	if oldest, next := l.Bounds(); oldest != 0 || next != 3 {
		t.Errorf("Bounds() = (%d, %d), want (0, 3): append after close must not extend the log",
			oldest, next)
	}
}

func TestEmptyAppendIsNoop(t *testing.T) {
	l := New[seq.Log](64)
	l.Append(nil)
	l.Append([]byte{})
	if oldest, next := l.Bounds(); oldest != 0 || next != 0 {
		t.Errorf("Bounds() = (%d, %d), want (0, 0)", oldest, next)
	}
}

// Closing a reader while another goroutine is blocked in Next must not crash.
//
// This is the shape of a client detaching: the service reads output on one goroutine and closes the
// reader from another when the stream ends, so the two run concurrently every single time. The bug
// was a nil dereference in Next, because Close observed sub outside the lock and cleared it after,
// leaving a window where Next saw a non-nil sub and then dereferenced nil. It took down the whole
// server, since a panic on that goroutine is not recovered.
//
// Run with -race to catch the unsynchronized access as well as the crash.
func TestCloseWhileBlockedInNext(t *testing.T) {
	// Many iterations, because the window is a few instructions wide. A single pass passes even with
	// the bug present.
	for range 200 {
		log := New[seq.Log](1024)
		r := log.Subscribe(0)

		// Blocked in Next: nothing has been appended, so it is waiting on the subscriber channel.
		done := make(chan error, 1)
		go func() {
			_, err := r.Next(context.Background())
			done <- err
		}()

		// Give the goroutine a chance to reach the blocking select before closing under it.
		runtime.Gosched()
		r.Close()

		select {
		case err := <-done:
			// ErrClosed is the contract: a closed reader has nothing further coming. Any other
			// error is acceptable too, but returning a chunk would mean it read a subscription that
			// was already gone.
			if err == nil {
				t.Fatal("Next() returned a chunk after Close(), want an error")
			}
			if !errors.Is(err, ErrClosed) {
				t.Fatalf("Next() error = %v, want ErrClosed", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("Next() did not return after the reader was closed")
		}
	}
}

// Close has to stay safe to call repeatedly, since the service closes readers from more than one
// exit path and both can run.
func TestReaderCloseIsIdempotentUnderConcurrency(t *testing.T) {
	log := New[seq.Log](1024)
	r := log.Subscribe(0)

	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r.Close()
		}()
	}
	wg.Wait()

	if _, err := r.Next(context.Background()); !errors.Is(err, ErrClosed) {
		t.Errorf("Next() error = %v after Close(), want ErrClosed", err)
	}
}
