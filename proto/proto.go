// Package proto holds the wire types of pocketgo and its Go client. The
// service and every client import it; no client declares its own copy of a
// wire type (ADR-0006). The contract is docs/specs/protocol.md.
package proto

// Version is the protocol version. A client sends it with every request in
// the HeaderProtocol header, and the service refuses a version it does not
// speak with 400 and ErrorCodeProtocol. Version 2 added the stream's hello,
// a session event after each entry, submission ids on entries, the
// session's queue in its live state and models' thinking levels: a client
// of one cannot work with a service of the other, so each refuses the
// other.
const Version = 2

// HeaderProtocol carries Version on every request.
const HeaderProtocol = "Pocket-Protocol"

// The request paths of protocol version 2. A path with {id} takes a session
// or approval id in that segment.
const (
	PathHello        = "/v1/hello"
	PathModels       = "/v1/models"
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
