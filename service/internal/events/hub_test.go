package events

import (
	"errors"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

var start = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

func mustHub(t *testing.T, capacity int, at time.Time) *Hub {
	t.Helper()
	h, err := NewHub(capacity, at, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

func seqs(events []proto.Event) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}

func TestPublishNumbersFromTheStartTime(t *testing.T) {
	h := mustHub(t, 4, start)
	e := h.Publish(proto.Event{Type: proto.EventLive})
	if e.Seq != start.UnixMilli()*1000 {
		t.Fatalf("seq %d", e.Seq)
	}
	later := mustHub(t, 4, start.Add(time.Millisecond))
	if next := later.Publish(proto.Event{}); next.Seq <= e.Seq {
		t.Fatalf("a later hub numbers below an earlier one: %d <= %d", next.Seq, e.Seq)
	}
}

func TestSubscribeReplaysWhatWasMissed(t *testing.T) {
	h := mustHub(t, 4, start)
	var published []proto.Event
	for range 3 {
		published = append(published, h.Publish(proto.Event{Type: proto.EventLive}))
	}
	backlog, _, cancel := h.Subscribe(published[0].Seq)
	defer cancel()
	if got := seqs(backlog); len(got) != 2 || got[0] != published[1].Seq || got[1] != published[2].Seq {
		t.Fatalf("backlog %v", got)
	}
	if backlog, _, cancel := h.Subscribe(published[2].Seq); len(backlog) != 0 {
		t.Fatalf("up to date: %v", seqs(backlog))
	} else {
		cancel()
	}
	if backlog, _, cancel := h.Subscribe(0); len(backlog) != 0 {
		t.Fatalf("from now: %v", seqs(backlog))
	} else {
		cancel()
	}
}

func TestSubscribeResyncsWhenEventsAreGone(t *testing.T) {
	h := mustHub(t, 2, start)
	first := h.Publish(proto.Event{})
	for range 3 {
		h.Publish(proto.Event{})
	}
	backlog, _, cancel := h.Subscribe(first.Seq)
	defer cancel()
	if len(backlog) != 1 || backlog[0].Type != proto.EventResync {
		t.Fatalf("backlog %+v", backlog)
	}
	future, _, cancel2 := h.Subscribe(first.Seq + 100)
	defer cancel2()
	if len(future) != 1 || future[0].Type != proto.EventResync {
		t.Fatalf("future id: %+v", future)
	}
	restarted := mustHub(t, 2, start.Add(time.Second))
	restarted.Publish(proto.Event{})
	old, _, cancel3 := restarted.Subscribe(first.Seq)
	defer cancel3()
	if len(old) != 1 || old[0].Type != proto.EventResync {
		t.Fatalf("id from before a restart: %+v", old)
	}
}

func TestSubscribersReceiveAndSlowOnesAreDropped(t *testing.T) {
	h := mustHub(t, 8, start)
	_, fast, cancelFast := h.Subscribe(0)
	defer cancelFast()
	_, slow, cancelSlow := h.Subscribe(0)
	defer cancelSlow()
	for range subscriberBuffer + 1 {
		h.Publish(proto.Event{})
		<-fast
	}
	n := 0
	for range slow {
		n++
	}
	if n != subscriberBuffer {
		t.Fatalf("the slow subscriber got %d events before it was dropped", n)
	}
}

func TestCancelClosesTheChannelOnce(t *testing.T) {
	h := mustHub(t, 2, start)
	_, ch, cancel := h.Subscribe(0)
	cancel()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel open after cancel")
	}
	h.Publish(proto.Event{}) // no panic on a cancelled subscriber
}

func TestRingKeepsTheNewestEvents(t *testing.T) {
	h := mustHub(t, 3, start)
	var published []proto.Event
	for range 10 {
		published = append(published, h.Publish(proto.Event{}))
	}
	backlog, _, cancel := h.Subscribe(published[6].Seq)
	defer cancel()
	if got := seqs(backlog); len(got) != 3 || got[0] != published[7].Seq || got[2] != published[9].Seq {
		t.Fatalf("backlog %v", got)
	}
	gone, _, cancel2 := h.Subscribe(published[5].Seq)
	defer cancel2()
	if len(gone) != 1 || gone[0].Type != proto.EventResync {
		t.Fatalf("an id before the ring: %+v", gone)
	}
}

func TestReservationsKeepNumbersGrowing(t *testing.T) {
	var high int64
	reserve := func(floor, block int64) (int64, error) {
		start := max(floor, high)
		high = start + block
		return start, nil
	}
	first, err := NewHub(4, start, reserve, nil)
	if err != nil {
		t.Fatal(err)
	}
	a := first.Publish(proto.Event{})
	// A restart with the clock set back still numbers above the old process.
	second, _ := NewHub(4, start.Add(-time.Hour), reserve, nil)
	if b := second.Publish(proto.Event{}); b.Seq <= a.Seq {
		t.Fatalf("after a clock step back: %d <= %d", b.Seq, a.Seq)
	}
	// Running out of a block reserves the next one.
	h, _ := NewHub(4, start, reserve, nil)
	h.limit = h.seq + 2
	before := high
	h.Publish(proto.Event{})
	h.Publish(proto.Event{})
	if high <= before {
		t.Fatal("no new block was reserved")
	}
}

func TestSubscribersCounts(t *testing.T) {
	h := mustHub(t, 2, start)
	_, _, cancel := h.Subscribe(0)
	if h.Subscribers() != 1 {
		t.Fatal("one subscriber expected")
	}
	cancel()
	if h.Subscribers() != 0 {
		t.Fatal("cancel did not remove the subscriber")
	}
}

func TestAFailedReservationDropsTheEventAndResyncs(t *testing.T) {
	var fail bool
	var high int64
	reserve := func(floor, block int64) (int64, error) {
		if fail {
			return 0, errors.New("disk full")
		}
		start := max(floor, high)
		high = start + block
		return start, nil
	}
	var reported []error
	h, err := NewHub(8, start, reserve, func(err error) { reported = append(reported, err) })
	if err != nil {
		t.Fatal(err)
	}
	first := h.Publish(proto.Event{})
	_, stream, cancel := h.Subscribe(0)
	defer cancel()
	h.limit = h.seq + 1 // the block is used up
	fail = true
	lost := h.Publish(proto.Event{})
	if lost.Seq != 0 || len(reported) != 1 {
		t.Fatalf("lost event seq %d, reported %v", lost.Seq, reported)
	}
	if _, open := <-stream; open {
		t.Fatal("a stream outlived the lost event")
	}
	if high > first.Seq+1+reserveBlock {
		t.Fatal("a number beyond the reservation was used")
	}
	fail = false
	next := h.Publish(proto.Event{})
	if next.Seq <= first.Seq {
		t.Fatalf("numbering after recovery %d <= %d", next.Seq, first.Seq)
	}
	backlog, _, cancel2 := h.Subscribe(first.Seq)
	defer cancel2()
	if len(backlog) != 1 || backlog[0].Type != proto.EventResync {
		t.Fatalf("a client from before the loss gets %+v", backlog)
	}
}
