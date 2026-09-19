package client

import (
	"errors"
	"reflect"
	"testing"

	serverv1 "github.com/chancez/cm/proto/cm/server/v1"
)

func outputMsg(seq uint64, data []byte) outMsg {
	return outMsg{resp: &serverv1.AttachResponse{
		Event: &serverv1.AttachResponse_Output{Output: &serverv1.Output{Seq: seq, Data: data}},
	}}
}

// The queue hands messages back in arrival order, whole. Order is the load-bearing part: a query, a
// resize reply and the output around them are only meaningful in the sequence the server sent them.
func TestOutQueueIsFIFO(t *testing.T) {
	q := newOutQueue()
	first := outputMsg(1, []byte("one"))
	second := outputMsg(4, []byte("two"))
	end := outMsg{err: errors.New("stream ended")}

	q.push(first)
	q.push(second)
	q.push(end)

	want := []outMsg{first, second, end}
	var got []outMsg
	for {
		m, ok := q.pop()
		if !ok {
			break
		}
		got = append(got, m)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("popped = %+v, want %+v", got, want)
	}
}

// Every message queued gets a signal, so an attach loop handling one per pass of its select is woken
// again for the rest. Without the re-signal in pop, a burst of chunks would sit in the queue until the
// next unrelated wake-up, which shows up as output arriving in fits.
func TestOutQueueSignalsOncePerMessage(t *testing.T) {
	q := newOutQueue()
	q.push(outputMsg(1, []byte("a")))
	q.push(outputMsg(2, []byte("b")))

	for i := range 2 {
		select {
		case <-q.wake:
		default:
			t.Fatalf("no signal waiting for message %d", i+1)
		}
		if _, ok := q.pop(); !ok {
			t.Fatalf("pop() found nothing for message %d", i+1)
		}
	}
	select {
	case <-q.wake:
		t.Error("a signal remains after the queue was drained")
	default:
	}
}

// A signal can outlive the message it announced, since pop takes whatever is there. That must read as
// "nothing to do" rather than as the stream having ended, which is what the channel this replaced meant
// by a closed receive.
func TestOutQueuePopOnAnEmptyQueue(t *testing.T) {
	q := newOutQueue()
	if m, ok := q.pop(); ok {
		t.Errorf("pop() = %+v, true, want the zero message and false", m)
	}
}

// Past the cap the backlog is replaced by a single notice that output was dropped, which the attach loop
// turns into a repaint. The cap is what keeps a client that has stalled for minutes -- one holding the
// session picker open -- from growing without bound.
func TestOutQueueDropsPastItsCap(t *testing.T) {
	q := newOutQueue()
	chunk := make([]byte, 64<<10)
	// One more than the cap holds, so the last push is the one that trips it.
	pushes := maxOutBacklog/len(chunk) + 1
	for i := range pushes {
		q.push(outputMsg(uint64(i), chunk))
	}

	m, ok := q.pop()
	if !ok {
		t.Fatal("pop() found nothing after the cap was passed")
	}
	if want := (outMsg{dropped: true}); m != want {
		t.Errorf("pop() = %+v, want %+v: the backlog is replaced rather than trimmed", m, want)
	}
	if _, ok := q.pop(); ok {
		t.Error("something remains behind the dropped notice, want the backlog cleared")
	}
}

// And nothing is buffered afterwards. The consumer is by definition not reading while this is set, so
// queueing what a repaint is about to supersede is exactly how the memory bound would be lost.
func TestOutQueueDiscardsWhileARepaintIsOwed(t *testing.T) {
	q := newOutQueue()
	chunk := make([]byte, 64<<10)
	for i := range maxOutBacklog/len(chunk) + 1 {
		q.push(outputMsg(uint64(i), chunk))
	}
	q.push(outputMsg(999, []byte("after")))

	if _, ok := q.pop(); !ok {
		t.Fatal("pop() found nothing, want the dropped notice")
	}
	if m, ok := q.pop(); ok {
		t.Errorf("pop() = %+v, want nothing queued after the drop", m)
	}
}

// The property the queue exists for: pushing never waits on the consumer. A test that hangs here is the
// failure, since a producer that waits is a ttrpc stream that gets closed a second later.
func TestOutQueuePushNeverBlocks(t *testing.T) {
	q := newOutQueue()
	chunk := make([]byte, 4<<10)
	for i := range 10000 {
		q.push(outputMsg(uint64(i), chunk))
	}
}
