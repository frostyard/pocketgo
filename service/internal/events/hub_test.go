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
	// The start's number marks where the hub starts; the first event is
	// the next.
	if e.Seq != start.UnixMilli()*1000+1 {
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
	_, backlog, _, cancel := h.Subscribe(published[0].Seq)
	defer cancel()
	if got := seqs(backlog); len(got) != 2 || got[0] != published[1].Seq || got[1] != published[2].Seq {
		t.Fatalf("backlog %v", got)
	}
	if _, backlog, _, cancel := h.Subscribe(published[2].Seq); len(backlog) != 0 {
		t.Fatalf("up to date: %v", seqs(backlog))
	} else {
		cancel()
	}
	if _, backlog, _, cancel := h.Subscribe(0); len(backlog) != 0 {
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
	_, backlog, _, cancel := h.Subscribe(first.Seq)
	defer cancel()
	if len(backlog) != 1 || backlog[0].Type != proto.EventResync {
		t.Fatalf("backlog %+v", backlog)
	}
	_, future, _, cancel2 := h.Subscribe(first.Seq + 100)
	defer cancel2()
	if len(future) != 1 || future[0].Type != proto.EventResync {
		t.Fatalf("future id: %+v", future)
	}
	restarted := mustHub(t, 2, start.Add(time.Second))
	restarted.Publish(proto.Event{})
	_, old, _, cancel3 := restarted.Subscribe(first.Seq)
	defer cancel3()
	if len(old) != 1 || old[0].Type != proto.EventResync {
		t.Fatalf("id from before a restart: %+v", old)
	}
}

func TestSubscribersReceiveAndSlowOnesAreDropped(t *testing.T) {
	h := mustHub(t, 8, start)
	_, _, fast, cancelFast := h.Subscribe(0)
	defer cancelFast()
	_, _, slow, cancelSlow := h.Subscribe(0)
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
	_, _, ch, cancel := h.Subscribe(0)
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
	_, backlog, _, cancel := h.Subscribe(published[6].Seq)
	defer cancel()
	if got := seqs(backlog); len(got) != 3 || got[0] != published[7].Seq || got[2] != published[9].Seq {
		t.Fatalf("backlog %v", got)
	}
	_, gone, _, cancel2 := h.Subscribe(published[5].Seq)
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
	_, _, _, cancel := h.Subscribe(0)
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
	_, _, stream, cancel := h.Subscribe(0)
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
	_, backlog, _, cancel2 := h.Subscribe(first.Seq)
	defer cancel2()
	if len(backlog) != 1 || backlog[0].Type != proto.EventResync {
		t.Fatalf("a client from before the loss gets %+v", backlog)
	}
}

// TestSubscribeStartsWhereTheClientStands checks the start of a
// subscription (protocol rule 18): the client's own Last-Event-ID, or the
// newest number for a client from now. Every event the subscription gets
// has a higher seq; a client that reconnects from start misses nothing, and
// one whose events are gone keeps its cursor until it has the resync.
func TestSubscribeStartsWhereTheClientStands(t *testing.T) {
	h := mustHub(t, 4, start)
	if from, backlog, _, cancel := h.Subscribe(0); from != h.seq || len(backlog) != 0 {
		t.Fatalf("before any event: start %d (newest %d), backlog %v", from, h.seq, seqs(backlog))
	} else {
		cancel()
	}
	var published []proto.Event
	for range 3 {
		published = append(published, h.Publish(proto.Event{Type: proto.EventLive}))
	}
	newest := published[2].Seq
	from, _, stream, cancel := h.Subscribe(0)
	if from != newest {
		t.Fatalf("a stream from now starts at %d, want the newest event %d", from, newest)
	}
	next := h.Publish(proto.Event{})
	if got := <-stream; got.Seq != next.Seq || got.Seq <= from {
		t.Fatalf("the next event %d after start %d", got.Seq, from)
	}
	cancel()
	// A reconnect from that start replays what came since.
	if again, backlog, _, cancel := h.Subscribe(from); again != from || len(backlog) != 1 || backlog[0].Seq != next.Seq {
		t.Fatalf("reconnect from %d: start %d, backlog %v", from, again, seqs(backlog))
	} else {
		cancel()
	}
	// A replay starts at the client's own id.
	if again, backlog, _, cancel := h.Subscribe(published[0].Seq); again != published[0].Seq || len(backlog) != 3 {
		t.Fatalf("replay: start %d, backlog %v", again, seqs(backlog))
	} else {
		cancel()
	}
	// Events that are gone: the start stays at the client's id, and the
	// resync carries the newest. A client that drops before the resync
	// comes back from the same id, and gets the resync again.
	for range 4 {
		h.Publish(proto.Event{})
	}
	for range 2 {
		again, backlog, _, cancel := h.Subscribe(published[0].Seq)
		cancel()
		if again != published[0].Seq || len(backlog) != 1 || backlog[0].Type != proto.EventResync || backlog[0].Seq != h.seq {
			t.Fatalf("gone: start %d, backlog %+v", again, backlog)
		}
	}
}

// TestAClientAtTheLostEventsBoundaryResyncs loses an event to a failed
// reservation right after the one a client saw last: the numbers did not
// move, but the client resyncs, also before any later event.
func TestAClientAtTheLostEventsBoundaryResyncs(t *testing.T) {
	fail := false
	var high int64
	reserve := func(floor, block int64) (int64, error) {
		if fail {
			return 0, errors.New("disk full")
		}
		start := max(floor, high)
		high = start + block
		return start, nil
	}
	h, err := NewHub(8, start, reserve, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := h.Publish(proto.Event{})
	if _, backlog, _, cancel := h.Subscribe(seen.Seq); len(backlog) != 0 {
		t.Fatalf("up to date: %+v", backlog)
	} else {
		cancel()
	}
	h.limit = h.seq + 1
	fail = true
	h.Publish(proto.Event{}) // lost
	_, backlog, _, cancel := h.Subscribe(seen.Seq)
	cancel()
	if len(backlog) != 1 || backlog[0].Type != proto.EventResync {
		t.Fatalf("a client at the lost event's boundary gets %+v", backlog)
	}
	fail = false
	after := h.Publish(proto.Event{})
	if _, backlog, _, cancel := h.Subscribe(after.Seq); len(backlog) != 0 {
		t.Fatalf("a client past the loss gets %+v", backlog)
	} else {
		cancel()
	}
}

// TestACursorOfAnEarlierProcessResyncs restarts the service after the old
// process used up its block, the clock behind: the new block starts right
// after the old process's last number, which a client may hold. That
// client resyncs, whatever the old process lost; a client of the new one
// is current.
func TestACursorOfAnEarlierProcessResyncs(t *testing.T) {
	var high int64
	reserve := func(floor, block int64) (int64, error) {
		start := max(floor, high)
		high = start + block
		return start, nil
	}
	old, _ := NewHub(4, start, reserve, nil)
	old.seq = old.limit - 2
	last := old.Publish(proto.Event{}) // the block's last number
	if last.Seq != high-1 {
		t.Fatalf("the old process's last number %d, its block ends at %d", last.Seq, high-1)
	}
	restarted, _ := NewHub(4, start.Add(-time.Hour), reserve, nil)
	_, backlog, _, cancel := restarted.Subscribe(last.Seq)
	cancel()
	if len(backlog) != 1 || backlog[0].Type != proto.EventResync {
		t.Fatalf("a client of the old process gets %+v", backlog)
	}
	from, _, _, cancel := restarted.Subscribe(0)
	cancel()
	if _, backlog, _, cancel := restarted.Subscribe(from); len(backlog) != 0 {
		t.Fatalf("a client of the new process, at its hello %d, gets %+v", from, backlog)
	} else {
		cancel()
	}
}
