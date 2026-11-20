// Package proto holds the wire types of pocketgo and its Go client. The
// service and every client import it; no client declares its own copy of a
// wire type (ADR-0006). The contract is docs/specs/protocol.md.
package proto

// Version is the protocol version. A client sends it with every request in
// the HeaderProtocol header, and the service refuses a version it does not
// speak with 400 and ErrorCodeProtocol. Version 2 added the stream's hello,
// a session event after each entry, submission ids on entries, the
// session's queue in its live state and models' thinking levels. Version 3
// added the agents' profiles (GET /v1/profiles), sessions started from a
// profile (CreateSession.Profile), and a session's profile and model route.
// Version 4 added threads and asks (ADR-0014): GET /v1/threads, POST
// /v1/threads/{id}/close, GET /v1/asks, POST /v1/asks/{id}, and their
// events. Version 5 added each agent's lessons (ADR-0015): GET
// /v1/lessons, POST /v1/lessons/{id}, and their events. Version 6 added
// the pull requests linked to threads (ADR-0016): GET /v1/pulls and their
// events. Version 7 added cross-review verdicts (ADR-0017): a pull
// request's ready-for-human label, its CI state and its newest verdict.
// Version 10 added plans, their tasks and consults (ADR-0020): GET
// /v1/plans, POST /v1/plans/{id}/approve, their events, and a thread's
// plan and task. A client of one version cannot work with a service of
// another, so each refuses the other.
const Version = 10

// HeaderProtocol carries Version on every request.
const HeaderProtocol = "Pocket-Protocol"

// The request paths of protocol version 10. A path with {id} takes a session,
// approval, thread, ask, lesson or plan id in that segment.
const (
	PathHello        = "/v1/hello"
	PathModels       = "/v1/models"
	PathProfiles     = "/v1/profiles"
	PathSessions     = "/v1/sessions"
	PathSession      = "/v1/sessions/{id}"
	PathMessages     = "/v1/sessions/{id}/messages"
	PathAbort        = "/v1/sessions/{id}/abort"
	PathApprovals    = "/v1/approvals"
	PathApproval     = "/v1/approvals/{id}"
	PathThreads      = "/v1/threads"
	PathCloseThread  = "/v1/threads/{id}/close"
	PathAsks         = "/v1/asks"
	PathAsk          = "/v1/asks/{id}"
	PathLessons      = "/v1/lessons"
	PathLesson       = "/v1/lessons/{id}"
	PathPulls        = "/v1/pulls"
	PathPull         = "/v1/pulls/{id}"
	PathPlans        = "/v1/plans"
	PathApprovePlan  = "/v1/plans/{id}/approve"
	PathEvents       = "/v1/events"
	MaxRequestIDSize = 128
	MaxMessageSize   = 256 * 1024
	// MaxQueuedListed is the most queued messages a live state lists, and
	// MaxQueuedText the most bytes of each one's text it carries, so every
	// live event stays far below what a client reads in one event.
	MaxQueuedListed = 100
	MaxQueuedText   = 4 * 1024
	// MaxAnswer is the most characters an answer to an ask has.
	MaxAnswer = 2000
	// MaxLesson is the most characters a lesson's text has.
	MaxLesson = 300
)
