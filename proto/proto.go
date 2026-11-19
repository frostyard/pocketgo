// Package proto holds the wire types of pocketgo and its Go client. The
// service and every client import it; no client declares its own copy of a
// wire type (ADR-0006). The contract is docs/specs/protocol.md.
package proto

// Version is the protocol version. A client sends it with every request in
// the HeaderProtocol header, and the service refuses a version it does not
// speak with 400 and ErrorCodeProtocol.
const Version = 1

// HeaderProtocol carries Version on every request.
const HeaderProtocol = "Pocket-Protocol"

// The request paths of protocol version 1. A path with {id} takes a session
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
)
