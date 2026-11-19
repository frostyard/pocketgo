package proto

// EventType names an event of GET /v1/events.
type EventType string

const (
	// EventHello is the first event of every stream. Its Seq is where the
	// stream starts: every later event of the stream has a higher Seq, and
	// none after it is missing. A client that sends it as Last-Event-ID
	// when it reconnects misses nothing.
	EventHello EventType = "hello"
	// EventSession carries a session that was created or changed.
	EventSession EventType = "session"
	// EventEntry carries a transcript entry that was written.
	EventEntry EventType = "entry"
	// EventLive carries a session's whole live state.
	EventLive EventType = "live"
	// EventApproval carries an approval that was requested or decided.
	EventApproval EventType = "approval"
	// EventThread carries a thread that was delegated or changed.
	EventThread EventType = "thread"
	// EventAsk carries an ask that was put to the owner or answered.
	EventAsk EventType = "ask"
	// EventLesson carries a lesson that was written, changed or decided.
	EventLesson EventType = "lesson"
	// EventPull carries a pull request that was linked to its thread, or
	// changed on GitHub.
	EventPull EventType = "pull"
	// EventResync tells the client that events were lost (its Last-Event-ID
	// is older than the service keeps): it refetches what it shows.
	EventResync EventType = "resync"
)

// Event is one Server-Sent Event of GET /v1/events. Its SSE id is Seq and its
// SSE event name is Type; its data is the JSON of this struct. Exactly one of
// the payload fields is set, matching Type; EventHello and EventResync have
// none.
type Event struct {
	Seq      int64        `json:"seq"`
	Type     EventType    `json:"type"`
	Session  *Session     `json:"session,omitempty"`
	Entry    *Entry       `json:"entry,omitempty"`
	Live     *Live        `json:"live,omitempty"`
	Approval *Approval    `json:"approval,omitempty"`
	Thread   *Thread      `json:"thread,omitempty"`
	Ask      *Ask         `json:"ask,omitempty"`
	Lesson   *Lesson      `json:"lesson,omitempty"`
	Pull     *PullRequest `json:"pull,omitempty"`
}
