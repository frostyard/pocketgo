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
// plan and task. Version 11 lists an API key's models like any other and
// adds a worker's routes worker-team and worker-asked (ADR-0022), which a
// version 10 client would not show as a worker's. A client of one version
// cannot work with a service of another, so each refuses the other.
// Version 12 adds a thread's shift (ADR-0024): POST
// /v1/threads/{id}/shift/stop and /resume, and the thread's shift.
// Version 13 adds the owner's terminals (ADR-0025): shells on the service
// host under /v1/terminals, and their event.
// Version 14 adds a profile's standing session (ADR-0026): a profile's
// and a session's standing, POST /v1/sessions/{id}/start-over, the
// compaction and reset entries, a live state's compaction and a model's
// context window.
// Version 15 names the session that started a thread's worker: a
// session's startedBy, the specialist's session that called team_worker.
// Version 16 starts a session on a repository, in a new git worktree when
// asked (ADR-0027): a profile's repositories carry their folders, and
// CreateSession its repository and worktree switch. Version 17 lets the
// owner change a session's model and thinking level (ADR-0028):
// POST /v1/sessions/{id}/model.
// Version 19 adds schedules (ADR-0030): GET /v1/schedules, the owner's
// schedules file and the supervisors' own, and their event.
// Version 20 adds a supervisor's message to a thread it delegated
// (ADR-0032): the sender of a user entry and of a queued message, which
// marks the supervisor's message apart from the owner's.
// Version 21 adds gathers and their proposals (ADR-0033): GET /v1/gathers,
// GET /v1/proposals, and their events. Version 22 attaches the owner's
// files to a message (ADR-0029): POST /v1/sessions/{id}/uploads,
// SendMessage's attachments, a user entry's images and files, a queued
// message's files and a model's images.
const Version = 22

// HeaderProtocol carries Version on every request.
const HeaderProtocol = "Pocket-Protocol"

// The request paths of protocol version 22. A path with {id} takes a session,
// approval, thread, ask, lesson, plan or terminal id in that segment.
// PathUploads takes a file the owner attaches to a message to the session
// (ADR-0029): its bytes are the body, its name and the request id are in
// the query string.
const (
	PathHello       = "/v1/hello"
	PathModels      = "/v1/models"
	PathProfiles    = "/v1/profiles"
	PathSessions    = "/v1/sessions"
	PathSession     = "/v1/sessions/{id}"
	PathMessages    = "/v1/sessions/{id}/messages"
	PathAbort       = "/v1/sessions/{id}/abort"
	PathStartOver   = "/v1/sessions/{id}/start-over"
	PathModel       = "/v1/sessions/{id}/model"
	PathUploads     = "/v1/sessions/{id}/uploads"
	PathApprovals   = "/v1/approvals"
	PathApproval    = "/v1/approvals/{id}"
	PathThreads     = "/v1/threads"
	PathCloseThread = "/v1/threads/{id}/close"
	PathStopShift   = "/v1/threads/{id}/shift/stop"
	PathResumeShift = "/v1/threads/{id}/shift/resume"
	PathAsks        = "/v1/asks"
	PathAsk         = "/v1/asks/{id}"
	PathLessons     = "/v1/lessons"
	PathLesson      = "/v1/lessons/{id}"
	PathPulls       = "/v1/pulls"
	PathPull        = "/v1/pulls/{id}"
	PathPlans       = "/v1/plans"
	PathApprovePlan = "/v1/plans/{id}/approve"
	PathSchedules   = "/v1/schedules"
	PathGathers     = "/v1/gathers"
	PathProposals   = "/v1/proposals"
	PathEvents      = "/v1/events"
	// The owner's terminals (ADR-0025): the list and a start, one shell
	// (DELETE ends it), its output by a long poll, its keys and its size.
	PathTerminals      = "/v1/terminals"
	PathTerminal       = "/v1/terminals/{id}"
	PathTerminalOutput = "/v1/terminals/{id}/output"
	PathTerminalInput  = "/v1/terminals/{id}/input"
	PathTerminalResize = "/v1/terminals/{id}/resize"
	MaxRequestIDSize   = 128
	MaxMessageSize     = 256 * 1024
	// MaxUpload is the most bytes an uploaded file has (rule 10a).
	MaxUpload = 50 << 20
	// MaxComposedMessage is the most bytes of a message with its attached
	// files' lines (rule 10a): its entry event stays far below what a
	// client reads in one event.
	MaxComposedMessage = 1 << 20
	// MaxQueuedListed is the most queued messages a live state lists, and
	// MaxQueuedText the most bytes of each one's text it carries, so every
	// live event stays far below what a client reads in one event.
	MaxQueuedListed = 100
	MaxQueuedText   = 4 * 1024
	// MaxAnswer is the most characters an answer to an ask has.
	MaxAnswer = 2000
	// MaxLesson is the most characters a lesson's text has.
	MaxLesson = 300
	// MaxTerminals is the most shells that run at once (rule 56): a start
	// past it is refused.
	MaxTerminals = 4
	// MaxTerminalInput is the most bytes of keys one input POST carries.
	MaxTerminalInput = 32 * 1024
	// TerminalKept is how many bytes of a shell's output pocketd keeps,
	// and the most one answer of its output carries.
	TerminalKept = 256 * 1024
)
