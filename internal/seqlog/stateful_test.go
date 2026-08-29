package seqlog

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chancez/cm/internal/seq"
)

// Stateful property testing for the log, hand-rolled rather than taken from a library.
//
// The library version would be pgregory.net/rapid, and it was considered. What it buys over this is
// semantic shrinking of an operation sequence and less plumbing. What it costs is the repo's first
// test-only dependency, against nine direct dependencies today all of which are load-bearing. The
// shrinker below is delta debugging in about thirty lines and it was measured doing the job, so the
// dependency waits for evidence that it is not enough.
//
// Why the log rather than something higher up: this is where the expensive bugs were. A subscriber
// naming a position in the wrong numbering was clamped forward and silently skipped output, and the
// clamp is legitimate for a different case, so the two are indistinguishable from inside. The invariant
// below is what separates them.

// op is one operation against the log. A sequence of these is the generated program, and shrinking
// removes elements from it.
type op struct {
	kind string // "append", "subscribe", "read"
	// n is the payload size for append, the subscriber index for read, and the offset-from-next for
	// subscribe.
	n int
}

func (o op) String() string { return fmt.Sprintf("%s(%d)", o.kind, o.n) }

func showOps(ops []op) string {
	parts := make([]string, len(ops))
	for i, o := range ops {
		parts[i] = o.String()
	}
	return strings.Join(parts, " ")
}

// subState is what the checker knows about one subscriber.
type subState struct {
	r *Reader[seq.Log]
	// from is the position this subscriber asked for, kept so a read with nothing to return can be
	// skipped before it costs a timeout.
	from seq.Log
	// next is the position the next chunk must start at, unless a gap is flagged.
	next seq.Log
	// started is false until the first chunk, since a subscriber's first position is chosen by the log.
	started bool
}

// runOps executes a program against a real log and returns the first invariant violation, or "".
//
// The model is deliberately tiny: every byte ever appended, and the log's max. Everything else is
// derived, because a model that reimplements the log would fail in the same ways as the log.
func runOps(ops []op, max int) string {
	l := New[seq.Log](max)
	defer l.Close()

	var all []byte // every byte ever appended, so a chunk can be checked against the truth
	var subs []*subState

	for i, o := range ops {
		switch o.kind {
		case "append":
			// Content that identifies its own position, so a misplaced chunk is obvious rather than a
			// coincidence of repeated bytes.
			p := make([]byte, o.n)
			for j := range p {
				p[j] = byte('a' + (len(all)+j)%26)
			}
			l.Append(p)
			all = append(all, p...)

		case "subscribe":
			_, next := l.Bounds()
			from := next
			// Offsets before the start and past the end are both interesting: the first is a subscriber
			// whose bytes aged out, the second is the wrong-numbering case that skipped output silently.
			if o.n <= int(from) {
				from = from - seq.Log(o.n)
			}
			subs = append(subs, &subState{r: l.Subscribe(from), from: from})

		case "read":
			if len(subs) == 0 {
				continue
			}
			s := subs[o.n%len(subs)]
			// Skipped when the log has nothing past this reader, which is most reads in a generated
			// program. Without this check each empty read waits out the deadline and the suite took 63s
			// instead of under a second, for no extra coverage: a read with nothing to return exercises
			// only the timeout.
			// Safe for an unstarted reader too: the log only ever clamps a requested position forward,
			// so nothing appended past what it asked for means nothing to deliver.
			if _, next := l.Bounds(); (s.started && next <= s.next) || (!s.started && next <= s.from) {
				continue
			}
			// A short deadline rather than a blocking read: "nothing available" is still possible, since
			// the check above races nothing but is not exact for an unstarted reader, and it must not hang.
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			c, err := s.r.Next(ctx)
			cancel()
			if err != nil {
				if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrClosed) {
					continue
				}
				return fmt.Sprintf("op %d %s: Next() error = %v", i, o, err)
			}

			// The conservation law: a chunk's bytes are exactly the bytes that were appended at that
			// position. This is what a silent skip violates.
			end := int(c.Seq) + len(c.Data)
			if end > len(all) {
				return fmt.Sprintf("op %d %s: chunk at %d..%d is past the %d bytes ever appended",
					i, o, c.Seq, end, len(all))
			}
			if string(c.Data) != string(all[c.Seq:end]) {
				return fmt.Sprintf("op %d %s: chunk at %d = %q, want %q",
					i, o, c.Seq, c.Data, all[c.Seq:end])
			}

			// The continuity law: a subscriber's chunks are contiguous unless it was told otherwise. A
			// jump without Gap is output the reader will never learn it missed, which is the failure the
			// gap flag exists to convert into something recoverable.
			if s.started && !c.Gap && c.Seq != s.next {
				return fmt.Sprintf("op %d %s: chunk starts at %d, want %d, and Gap is not set: "+
					"%d bytes vanished silently", i, o, c.Seq, s.next, int(c.Seq)-int(s.next))
			}
			if s.started && c.Seq < s.next {
				return fmt.Sprintf("op %d %s: chunk starts at %d, behind the reader's position %d: "+
					"output would be delivered twice", i, o, c.Seq, s.next)
			}
			s.started = true
			s.next = seq.Log(end)
		}
	}
	return ""
}

// shrink reduces a failing program to a minimal one that still fails the same way.
//
// Delta debugging: try removing each operation in turn, keep any removal that still reproduces, repeat
// until a pass changes nothing. This is the part a property-testing library would provide, and it is the
// part that makes a failure readable. Measured against two mutations of reader.go, dropping the gap flag
// on a clamp and reporting a chunk one byte late: 60 operations shrink to 6 and to 4, and the short form
// names the bug where the long one buries it.
//
// "The same way" is load-bearing. Delta debugging that accepts any failure will happily slide onto a
// different bug and report a minimal program for something other than what was found, so the reason is
// compared rather than just the fact of failing.
func shrink(ops []op, max int, want string) []op {
	best := ops
	for progress := true; progress; {
		progress = false
		for i := range best {
			candidate := make([]op, 0, len(best)-1)
			candidate = append(candidate, best[:i]...)
			candidate = append(candidate, best[i+1:]...)
			if len(candidate) == 0 {
				continue
			}
			if reason(runOps(candidate, max)) == reason(want) {
				best = candidate
				progress = true
				break
			}
		}
	}
	return best
}

// reason strips the "op N kind(n): " prefix from a violation, so two reports of the same defect compare
// equal even though removing an operation renumbers the ones after it.
func reason(problem string) string {
	if problem == "" {
		return ""
	}
	if _, rest, ok := strings.Cut(problem, ": "); ok {
		return rest
	}
	return problem
}

// TestLogStatefulProperties runs generated programs against the log.
//
// Seeded and deterministic, and in the ordinary test run rather than behind -fuzz, which is the point: a
// fuzz target only replays its corpus unless someone remembers the flag, while this explores on every
// `go test`. The seeds are fixed so a failure reproduces exactly; widening the search is a matter of
// adding seeds rather than of luck.
func TestLogStatefulProperties(t *testing.T) {
	// A small max on purpose. Trimming is what makes positions age out, and a log large enough never to
	// trim exercises none of the interesting paths.
	for _, max := range []int{4, 16, 64} {
		for seed := int64(1); seed <= 40; seed++ {
			t.Run(fmt.Sprintf("max%d/seed%d", max, seed), func(t *testing.T) {
				ops := generate(rand.New(rand.NewSource(seed)), 60)
				if problem := runOps(ops, max); problem != "" {
					minimal := shrink(ops, max, problem)
					t.Fatalf("%s\n\nminimal failing program (%d ops, shrunk from %d):\n  %s\n\n"+
						"reproduce with: max=%d, ops above",
						runOps(minimal, max), len(minimal), len(ops), showOps(minimal), max)
				}
			})
		}
	}
}

// generate builds a random program.
//
// Weighted towards appends and reads, since those are what move the log, with subscribes rarer because
// each one adds a reader whose whole history has to stay consistent.
func generate(rng *rand.Rand, n int) []op {
	ops := make([]op, 0, n)
	// Always start with a subscriber, or a program of pure appends checks nothing.
	ops = append(ops, op{kind: "subscribe", n: 0})
	for len(ops) < n {
		switch rng.Intn(10) {
		case 0, 1, 2, 3:
			ops = append(ops, op{kind: "append", n: 1 + rng.Intn(20)})
		case 4:
			// An offset back from the end, so some subscribers ask for positions that have aged out.
			ops = append(ops, op{kind: "subscribe", n: rng.Intn(30)})
		default:
			ops = append(ops, op{kind: "read", n: rng.Intn(4)})
		}
	}
	return ops
}

// FuzzLogStatefulProperties is the same checker driven by a coverage-guided search rather than by seeds.
//
// The input is read as a program, three bytes per operation. Both forms are here because they fail
// differently: the seeded sweep above runs on every `go test` and cannot regress unnoticed, while this
// one goes deeper but only under -fuzz, and a corpus entry it finds becomes a permanent seed. Neither
// alone is enough, since a fuzz target nobody runs with the flag is a test that only replays its corpus.
func FuzzLogStatefulProperties(f *testing.F) {
	// The two programs the mutations above shrank to, as seeds: an aged-out subscriber and a
	// position-reporting error are the two shapes worth keeping in the corpus.
	f.Add([]byte{1, 3, 0, 0, 4, 0, 2, 2, 0, 0, 13, 0, 2, 1, 0}, 4)
	f.Add([]byte{0, 13, 0, 1, 7, 0, 2, 1, 0}, 4)

	f.Fuzz(func(t *testing.T, program []byte, max int) {
		// A log has to be able to hold something, and an enormous one never trims, which is where the
		// interesting paths are.
		if max < 1 || max > 4096 {
			return
		}
		ops := decode(program)
		if len(ops) == 0 {
			return
		}
		if problem := runOps(ops, max); problem != "" {
			minimal := shrink(ops, max, problem)
			t.Fatalf("%s\n\nminimal failing program (%d ops, shrunk from %d):\n  %s\n\nmax=%d",
				runOps(minimal, max), len(minimal), len(ops), showOps(minimal), max)
		}
	})
}

// decode reads a byte slice as a program, three bytes per operation: one for the kind and two for n.
//
// A fixed width rather than a variable encoding, so that flipping one byte changes one operation instead
// of reframing everything after it. That keeps the fuzzer's mutations meaningful and its corpus stable.
func decode(program []byte) []op {
	var ops []op
	for i := 0; i+2 < len(program); i += 3 {
		n := int(program[i+1])<<8 | int(program[i+2])
		switch program[i] % 3 {
		case 0:
			// Bounded, since a single huge append trims everything and tests only the trim.
			ops = append(ops, op{kind: "append", n: 1 + n%64})
		case 1:
			ops = append(ops, op{kind: "subscribe", n: n % 128})
		default:
			ops = append(ops, op{kind: "read", n: n % 8})
		}
	}
	// A program of pure appends checks nothing, so give it a subscriber. Prepended rather than rejected:
	// rejecting would throw away most of the fuzzer's inputs.
	if len(ops) > 0 && ops[0].kind != "subscribe" {
		ops = append([]op{{kind: "subscribe", n: 0}}, ops...)
	}
	return ops
}

// TestLogStatefulPropertiesConcurrently runs the same invariants with each reader on its own goroutine.
//
// The sequential form above cannot see a race, and the races are what hurt here: reader.go's Close
// carries a comment about a detaching client crashing the server on a nil dereference, and that window
// was a few instructions wide. Those specific cases have unit tests next door, which is the right place
// for a known bug. This is for the ones nobody has named yet, so it is worth running under -race.
//
// Appends stay on this goroutine on purpose. That keeps the record of what was appended exact without a
// lock on the write side, so the readers check a real oracle rather than a guess, and the only
// concurrency is the part under test. A test that has to approximate its own expectation cannot fail
// usefully.
func TestLogStatefulPropertiesConcurrently(t *testing.T) {
	const readers = 4
	const appends = 200

	l := New[seq.Log](64)
	defer l.Close()

	var mu sync.RWMutex // guards all, read by the readers, written here
	var all []byte

	// Every reader subscribes before anything is appended, so each one's stream starts at 0 and any
	// discontinuity is either a flagged gap or a defect.
	type result struct {
		problem string
	}
	results := make(chan result, readers)
	var wg sync.WaitGroup
	for i := 0; i < readers; i++ {
		r := l.Subscribe(0)
		wg.Add(1)
		go func() {
			defer wg.Done()
			defer r.Close()
			var next seq.Log
			var started bool
			for {
				ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
				c, err := r.Next(ctx)
				cancel()
				if err != nil {
					// Either the log closed or the writer finished. Both end this reader.
					results <- result{}
					return
				}

				mu.RLock()
				end := int(c.Seq) + len(c.Data)
				var problem string
				switch {
				case end > len(all):
					problem = fmt.Sprintf("chunk at %d..%d is past the %d bytes appended so far",
						c.Seq, end, len(all))
				case string(c.Data) != string(all[c.Seq:end]):
					problem = fmt.Sprintf("chunk at %d = %q, want %q", c.Seq, c.Data, all[c.Seq:end])
				case started && !c.Gap && c.Seq != next:
					problem = fmt.Sprintf("chunk starts at %d, want %d, and Gap is not set: %d bytes "+
						"vanished silently", c.Seq, next, int(c.Seq)-int(next))
				case started && c.Seq < next:
					problem = fmt.Sprintf("chunk starts at %d, behind the reader's position %d: output "+
						"would be delivered twice", c.Seq, next)
				}
				mu.RUnlock()

				if problem != "" {
					results <- result{problem: problem}
					return
				}
				started, next = true, seq.Log(end)
			}
		}()
	}

	for i := 0; i < appends; i++ {
		p := []byte(fmt.Sprintf("<%03d>", i))
		mu.Lock()
		l.Append(p)
		all = append(all, p...)
		mu.Unlock()
		// Yield rather than sleep, so readers interleave without the test taking a wall-clock second per
		// append. A tight loop with no yield lets the writer finish before any reader starts, which turns
		// this into the sequential test with extra machinery.
		runtime.Gosched()
	}

	// Closing the log is what ends the readers, and it is also the shutdown path where the nil
	// dereference lived.
	l.Close()
	wg.Wait()
	close(results)

	for r := range results {
		if r.problem != "" {
			t.Errorf("%s", r.problem)
		}
	}
}
