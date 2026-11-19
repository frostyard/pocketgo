// Package events numbers the service's events and fans them out to the
// clients of GET /v1/events. It keeps the newest events so a client that
// reconnects with Last-Event-ID gets what it missed, or a resync when it
// missed more than the hub keeps.
package events

import (
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// subscriberBuffer is how many events a subscriber may fall behind before
// the hub drops it; the client reconnects with Last-Event-ID.
const subscriberBuffer = 256

// Hub is safe for concurrent use.
type Hub struct {
	mu       sync.Mutex
	seq      int64
	first    int64 // seq of kept[0]
	kept     []proto.Event
	capacity int
	subs     map[*subscriber]struct{}
}

type subscriber struct {
	ch     chan proto.Event
	closed bool
}

// NewHub returns a hub that keeps capacity events. Seq numbers start above
// start's milliseconds times 1000, so they keep growing across restarts and a
// client's Last-Event-ID from before a restart is older than every kept event.
func NewHub(capacity int, start time.Time) *Hub {
	return &Hub{seq: start.UnixMilli() * 1000, capacity: capacity, subs: map[*subscriber]struct{}{}}
}

// Publish numbers event, keeps it and sends it to every subscriber. A
// subscriber that has fallen subscriberBuffer events behind is dropped.
func (h *Hub) Publish(event proto.Event) proto.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.seq++
	event.Seq = h.seq
	if len(h.kept) == 0 {
		h.first = event.Seq
	}
	h.kept = append(h.kept, event)
	if len(h.kept) > h.capacity {
		drop := len(h.kept) - h.capacity
		h.kept = append(h.kept[:0:0], h.kept[drop:]...)
		h.first += int64(drop)
	}
	for sub := range h.subs {
		select {
		case sub.ch <- event:
		default:
			h.dropLocked(sub)
		}
	}
	return event
}

func (h *Hub) dropLocked(sub *subscriber) {
	if !sub.closed {
		sub.closed = true
		close(sub.ch)
	}
	delete(h.subs, sub)
}

// Subscribe returns the kept events after lastEventID and a channel of the
// events published from now on. lastEventID 0 means "from now". When events
// after lastEventID are no longer kept, or lastEventID is in the future,
// backlog is a single EventResync. The channel closes when cancel is called
// or the subscriber falls behind.
func (h *Hub) Subscribe(lastEventID int64) (backlog []proto.Event, events <-chan proto.Event, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	switch {
	case lastEventID == 0 || lastEventID == h.seq:
	case lastEventID > h.seq || len(h.kept) == 0 || lastEventID < h.first-1:
		backlog = []proto.Event{{Seq: h.seq, Type: proto.EventResync}}
	default:
		backlog = append(backlog, h.kept[lastEventID-h.first+1:]...)
	}
	sub := &subscriber{ch: make(chan proto.Event, subscriberBuffer)}
	h.subs[sub] = struct{}{}
	return backlog, sub.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.dropLocked(sub)
	}
}
