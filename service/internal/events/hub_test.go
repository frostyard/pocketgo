package events

import (
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

var start = time.Date(2026, 11, 18, 0, 0, 0, 0, time.UTC)

func seqs(events []proto.Event) []int64 {
	out := make([]int64, len(events))
	for i, e := range events {
		out[i] = e.Seq
	}
	return out
}

func TestPublishNumbersFromTheStartTime(t *testing.T) {
	h := NewHub(4, start)
	e := h.Publish(proto.Event{Type: proto.EventLive})
	if e.Seq != start.UnixMilli()*1000+1 {
		t.Fatalf("seq %d", e.Seq)
	}
	later := NewHub(4, start.Add(time.Millisecond))
	if next := later.Publish(proto.Event{}); next.Seq <= e.Seq {
		t.Fatalf("a later hub numbers below an earlier one: %d <= %d", next.Seq, e.Seq)
	}
}

func TestSubscribeReplaysWhatWasMissed(t *testing.T) {
	h := NewHub(4, start)
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
	h := NewHub(2, start)
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
	restarted := NewHub(2, start.Add(time.Second))
	restarted.Publish(proto.Event{})
	old, _, cancel3 := restarted.Subscribe(first.Seq)
	defer cancel3()
	if len(old) != 1 || old[0].Type != proto.EventResync {
		t.Fatalf("id from before a restart: %+v", old)
	}
}

func TestSubscribersReceiveAndSlowOnesAreDropped(t *testing.T) {
	h := NewHub(8, start)
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
	h := NewHub(2, start)
	_, ch, cancel := h.Subscribe(0)
	cancel()
	cancel()
	if _, ok := <-ch; ok {
		t.Fatal("channel open after cancel")
	}
	h.Publish(proto.Event{}) // no panic on a cancelled subscriber
}
