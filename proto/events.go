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
	// EventAsk carries an ask that was put to the owner, answered, or
	// withdrawn by a supervisor's message (ADR-0037).
	EventAsk EventType = "ask"
	// EventLesson carries a lesson that was written, changed or decided.
	EventLesson EventType = "lesson"
	// EventPull carries a pull request that was linked to its thread, or
	// changed on GitHub.
	EventPull EventType = "pull"
	// EventPlan carries a plan that started or changed: its text, its
	// gate, its tasks or its consults.
	EventPlan EventType = "plan"
	// EventTerminals carries the whole list of the owner's terminals, after
	// a shell starts, exits, ends or leaves the list (ADR-0025).
	EventTerminals EventType = "terminals"
	// EventSchedules says the schedules changed: one went out, was added,
	// cancelled or changed, or the owner's schedules file changed what the
	// list says (ADR-0030). It carries no list, which the client fetches
	// with GET /v1/schedules, so the events pocketd keeps for clients that
	// come back stay small.
	EventSchedules EventType = "schedules"
	// EventGather carries a gather that started or changed: an ask's state,
	// a proposal's count, or its end (ADR-0033).
	EventGather EventType = "gather"
	// EventProposal carries a proposal that was filed, changed, ordered or
	// dropped (ADR-0033).
	EventProposal EventType = "proposal"
	// EventLaunch carries a launch that started or changed: an item's start
	// or skip, or its end (ADR-0034).
	EventLaunch EventType = "launch"
	// EventResync tells the client that events were lost (its Last-Event-ID
	// is older than the service keeps): it refetches what it shows.
	EventResync EventType = "resync"
)

// Event is one Server-Sent Event of GET /v1/events. Its SSE id is Seq and its
// SSE event name is Type; its data is the JSON of this struct. Exactly one of
// the payload fields is set, matching Type; EventHello, EventResync and
// EventSchedules have none, and EventTerminals leaves Terminals out when no
// shell is listed.
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
	Plan     *Plan        `json:"plan,omitempty"`
	// Terminals is the whole list, oldest first, as GET /v1/terminals
	// gives it.
	Terminals []Terminal `json:"terminals,omitempty"`
	Gather    *Gather    `json:"gather,omitempty"`
	Proposal  *Proposal  `json:"proposal,omitempty"`
	Launch    *Launch    `json:"launch,omitempty"`
}
