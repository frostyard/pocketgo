// Package events numbers the service's events and fans them out to the
// clients of GET /v1/events. It keeps the newest events in a ring, so a
// client that reconnects with Last-Event-ID gets what it missed, or a resync
// when it missed more than the hub keeps.
package events

import (
	"sync"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// subscriberBuffer is how many events a subscriber may fall behind before
// the hub drops it; the client reconnects with Last-Event-ID.
const subscriberBuffer = 256

// reserveBlock is how many event numbers the hub reserves at a time.
const reserveBlock = 1_000_000

// Reserver durably reserves the numbers [start, start+block) with start at
// least floor and above every number reserved before, and returns start.
// store.Store.ReserveSeq is one.
type Reserver func(floor, block int64) (int64, error)

// Hub is safe for concurrent use.
type Hub struct {
	mu      sync.Mutex
	seq     int64 // the last number given out
	limit   int64 // the first number not reserved
	reserve Reserver
	onError func(error)
	ring    []proto.Event
	head    int // index of the oldest kept event
	count   int
	subs    map[*subscriber]struct{}
}

type subscriber struct {
	ch     chan proto.Event
	closed bool
}

// NewHub returns a hub that keeps capacity events. Numbers start at or above
// start's milliseconds times 1000. With a reserve, they also start above
// every number an earlier process reserved, so they grow across restarts even
// when the clock moves back, and no number is given out unreserved: when a
// reservation fails (reported to onError), the event is dropped, every stream
// ends and the kept events are cleared, so each client resyncs.
func NewHub(capacity int, start time.Time, reserve Reserver, onError func(error)) (*Hub, error) {
	h := &Hub{ring: make([]proto.Event, capacity), reserve: reserve, onError: onError, subs: map[*subscriber]struct{}{}}
	floor := start.UnixMilli() * 1000
	h.seq, h.limit = floor-1, 0
	if reserve != nil {
		first, err := reserve(floor, reserveBlock)
		if err != nil {
			return nil, err
		}
		h.seq, h.limit = first-1, first+reserveBlock
	}
	if h.onError == nil {
		h.onError = func(error) {}
	}
	return h, nil
}

// next returns the next number, reserving another block when the reserved
// one runs out; ok is false when that reservation fails.
func (h *Hub) next() (int64, bool) {
	if h.reserve != nil && h.seq+1 >= h.limit {
		first, err := h.reserve(h.seq+1, reserveBlock)
		if err != nil {
			h.onError(err)
			return 0, false
		}
		h.seq, h.limit = first-1, first+reserveBlock
	}
	h.seq++
	return h.seq, true
}

// Publish numbers event, keeps it and sends it to every subscriber. A
// subscriber that has fallen subscriberBuffer events behind is dropped.
func (h *Hub) Publish(event proto.Event) proto.Event {
	h.mu.Lock()
	defer h.mu.Unlock()
	seq, ok := h.next()
	if !ok {
		// The event is lost: clients that saw what came before must resync.
		h.count = 0
		for sub := range h.subs {
			h.dropLocked(sub)
		}
		return event
	}
	event.Seq = seq
	if len(h.ring) > 0 {
		if h.count < len(h.ring) {
			h.ring[(h.head+h.count)%len(h.ring)] = event
			h.count++
		} else {
			h.ring[h.head] = event
			h.head = (h.head + 1) % len(h.ring)
		}
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

// kept returns the i-th oldest kept event.
func (h *Hub) kept(i int) proto.Event { return h.ring[(h.head+i)%len(h.ring)] }

// Subscribe returns where the subscription starts, the kept events after
// lastEventID and a channel of the events published from now on.
// lastEventID 0 means "from now". When events after lastEventID are no
// longer kept, or lastEventID is in the future, backlog is a single
// EventResync. The channel closes when cancel is called or the subscriber
// falls behind.
//
// start is the Seq of the stream's EventHello (protocol rule 18): every
// event of backlog and of the channel has a higher Seq, and none after it is
// missing. It is lastEventID when the backlog replays what came after it,
// and the newest number given out otherwise.
func (h *Hub) Subscribe(lastEventID int64) (start int64, backlog []proto.Event, events <-chan proto.Event, cancel func()) {
	h.mu.Lock()
	defer h.mu.Unlock()
	start = h.seq
	switch {
	case lastEventID == 0 || lastEventID == h.seq:
	case lastEventID > h.seq || h.count == 0 || lastEventID < h.kept(0).Seq-1:
		backlog = []proto.Event{{Seq: h.seq, Type: proto.EventResync}}
	default:
		start = lastEventID
		for i := range h.count {
			if e := h.kept(i); e.Seq > lastEventID {
				backlog = append(backlog, e)
			}
		}
	}
	sub := &subscriber{ch: make(chan proto.Event, subscriberBuffer)}
	h.subs[sub] = struct{}{}
	return start, backlog, sub.ch, func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.dropLocked(sub)
	}
}

// Subscribers is the number of open subscriptions; tests wait on it instead
// of sleeping.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.subs)
}
