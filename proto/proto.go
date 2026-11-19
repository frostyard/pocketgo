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
// A client of one version cannot work with a service of another, so each
// refuses the other.
const Version = 3

// HeaderProtocol carries Version on every request.
const HeaderProtocol = "Pocket-Protocol"

// The request paths of protocol version 3. A path with {id} takes a session
// or approval id in that segment.
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
	PathEvents       = "/v1/events"
	MaxRequestIDSize = 128
	MaxMessageSize   = 256 * 1024
	// MaxQueuedListed is the most queued messages a live state lists, and
	// MaxQueuedText the most bytes of each one's text it carries, so every
	// live event stays far below what a client reads in one event.
	MaxQueuedListed = 100
	MaxQueuedText   = 4 * 1024
)
