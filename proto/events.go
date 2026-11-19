package proto

// EventType names an event of GET /v1/events.
type EventType string

const (
	// EventSession carries a session that was created or changed.
	EventSession EventType = "session"
	// EventEntry carries a transcript entry that was written.
	EventEntry EventType = "entry"
	// EventLive carries a session's whole live state.
	EventLive EventType = "live"
	// EventApproval carries an approval that was requested or decided.
	EventApproval EventType = "approval"
	// EventResync tells the client that events were lost (its Last-Event-ID
	// is older than the service keeps): it refetches what it shows.
	EventResync EventType = "resync"
)

// Event is one Server-Sent Event of GET /v1/events. Its SSE id is Seq and its
// SSE event name is Type; its data is the JSON of this struct. Exactly one of
// the payload fields is set, matching Type; EventResync has none.
type Event struct {
	Seq      int64     `json:"seq"`
	Type     EventType `json:"type"`
	Session  *Session  `json:"session,omitempty"`
	Entry    *Entry    `json:"entry,omitempty"`
	Live     *Live     `json:"live,omitempty"`
	Approval *Approval `json:"approval,omitempty"`
}
