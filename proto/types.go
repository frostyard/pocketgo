package proto

import (
	"encoding/json"
	"time"
)

// Hello is the answer to GET /v1/hello: who the caller is and what the
// service speaks.
type Hello struct {
	Protocol int       `json:"protocol"`
	Server   string    `json:"server"`
	Device   string    `json:"device"`
	Now      time.Time `json:"now"`
}

// ModelRef names one model of one provider.
type ModelRef struct {
	Provider string `json:"provider"`
	ID       string `json:"id"`
}

// String is "provider/id".
func (m ModelRef) String() string { return m.Provider + "/" + m.ID }

// Model is a model the service can run now: its provider has a credential.
type Model struct {
	ModelRef
	Name string `json:"name"`
	// Reasoning says the model thinks: ThinkingLevels lists a level other
	// than "off".
	Reasoning bool `json:"reasoning"`
	// ThinkingLevels are the levels CreateSession.Thinking takes with this
	// model, lowest first, out of "off", "minimal", "low", "medium",
	// "high", "xhigh" and "max". A model that does not think lists only
	// "off".
	ThinkingLevels []string `json:"thinkingLevels"`
}

// Usage is tokens and cost, for one entry or summed over a session.
type Usage struct {
	Input      int64   `json:"input"`
	Output     int64   `json:"output"`
	CacheRead  int64   `json:"cacheRead"`
	CacheWrite int64   `json:"cacheWrite"`
	Total      int64   `json:"total"`
	Cost       float64 `json:"cost"`
}

// Add returns u plus v.
func (u Usage) Add(v Usage) Usage {
	return Usage{
		Input:      u.Input + v.Input,
		Output:     u.Output + v.Output,
		CacheRead:  u.CacheRead + v.CacheRead,
		CacheWrite: u.CacheWrite + v.CacheWrite,
		Total:      u.Total + v.Total,
		Cost:       u.Cost + v.Cost,
	}
}

// SessionState is what a session is doing.
type SessionState string

const (
	// SessionIdle: no run.
	SessionIdle SessionState = "idle"
	// SessionRunning: a run is in progress.
	SessionRunning SessionState = "running"
	// SessionWaiting: a run waits on the owner (an approval).
	SessionWaiting SessionState = "waiting"
)

// Session is one conversation with one agent.
type Session struct {
	ID       int64    `json:"id"`
	Title    string   `json:"title"`
	Cwd      string   `json:"cwd"`
	Model    ModelRef `json:"model"`
	Thinking string   `json:"thinking,omitempty"`
	// Profile is the profile the session started from; Route the rule that
	// chose its model: owner, profile or profile-alias.
	Profile   string       `json:"profile,omitempty"`
	Route     string       `json:"route,omitempty"`
	State     SessionState `json:"state"`
	Usage     Usage        `json:"usage"`
	CreatedAt time.Time    `json:"createdAt"`
	UpdatedAt time.Time    `json:"updatedAt"`
}

// EntryKind is the kind of a transcript entry.
type EntryKind string

const (
	EntryUser       EntryKind = "user"
	EntryAssistant  EntryKind = "assistant"
	EntryToolResult EntryKind = "toolResult"
	EntrySystem     EntryKind = "system"
	// EntryOther is an entry kind this protocol version does not model;
	// Entry.Raw names it.
	EntryOther EntryKind = "other"
)

// ToolCall is one tool call of an assistant entry.
type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

// ToolResult is the result of one tool call.
type ToolResult struct {
	CallID  string `json:"callId"`
	Name    string `json:"name"`
	IsError bool   `json:"isError"`
	Text    string `json:"text"`
}

// Entry is one entry of a session's transcript. Entry ids grow in the order
// entries are written; a transcript lists entries by ascending id.
type Entry struct {
	ID         int64       `json:"id"`
	SessionID  int64       `json:"sessionId"`
	Kind       EntryKind   `json:"kind"`
	Raw        string      `json:"raw,omitempty"`
	Text       string      `json:"text,omitempty"`
	Thinking   string      `json:"thinking,omitempty"`
	ToolCalls  []ToolCall  `json:"toolCalls,omitempty"`
	ToolResult *ToolResult `json:"toolResult,omitempty"`
	Model      *ModelRef   `json:"model,omitempty"`
	Usage      *Usage      `json:"usage,omitempty"`
	StopReason string      `json:"stopReason,omitempty"`
	Error      string      `json:"error,omitempty"`
	At         time.Time   `json:"at,omitzero"`

	// SubmissionID is, on a user entry, the submission that placed it: the
	// SentMessage.SubmissionID of the owner's message. 0 when no
	// submission placed the entry.
	SubmissionID int64 `json:"submissionId,omitempty"`
}

// ToolStatus is where a live tool call is.
type ToolStatus string

const (
	ToolPending ToolStatus = "pending"
	ToolRunning ToolStatus = "running"
	ToolDone    ToolStatus = "done"
)

// LiveTool is a tool call of the current round.
type LiveTool struct {
	CallID string     `json:"callId"`
	Name   string     `json:"name"`
	Status ToolStatus `json:"status"`
}

// QueuedMessage is a message the session holds for a run: taken, and not
// in the transcript yet. Text is the message's first MaxQueuedText bytes,
// cut at a character; Cut says it goes on. Its entry has all of it.
type QueuedMessage struct {
	SubmissionID int64  `json:"submissionId"`
	Text         string `json:"text"`
	Cut          bool   `json:"cut,omitempty"`
}

// Live is what a session's run is doing right now. It is replaced whole on
// every change.
type Live struct {
	SessionID int64      `json:"sessionId"`
	Busy      bool       `json:"busy"`
	Partial   *Entry     `json:"partial,omitempty"`
	Tools     []LiveTool `json:"tools,omitempty"`
	Failure   string     `json:"failure,omitempty"`

	// Queued are the messages that wait for a run, oldest first, at most
	// MaxQueuedListed; QueuedMore counts the newer ones it leaves out. A
	// message leaves the queue when a run reaches it and writes its entry,
	// or when an abort of a running session withdraws it; a run that fails
	// keeps it for the next.
	Queued     []QueuedMessage `json:"queued,omitempty"`
	QueuedMore int             `json:"queuedMore,omitempty"`
}

// ApprovalState is where an approval is.
type ApprovalState string

const (
	ApprovalPending ApprovalState = "pending"
	ApprovalAllowed ApprovalState = "allowed"
	ApprovalDenied  ApprovalState = "denied"
)

// Approval is a tool call that waits for the owner's decision.
type Approval struct {
	ID          string          `json:"id"`
	SessionID   int64           `json:"sessionId"`
	CallID      string          `json:"callId"`
	Tool        string          `json:"tool"`
	Arguments   json.RawMessage `json:"arguments"`
	State       ApprovalState   `json:"state"`
	RequestedAt time.Time       `json:"requestedAt"`
	DecidedAt   *time.Time      `json:"decidedAt,omitempty"`
	DecidedBy   string          `json:"decidedBy,omitempty"`
	Reason      string          `json:"reason,omitempty"`
}

// CreateSession is the body of POST /v1/sessions.
type CreateSession struct {
	RequestID string `json:"requestId"`
	Title     string `json:"title,omitempty"`
	// Profile starts the session as an agent (ADR-0013): its identity, its
	// tools, and by default its home as Cwd and its model as Model. Empty
	// for a plain session.
	Profile string `json:"profile,omitempty"`
	// Cwd may be empty with a Profile, for its home.
	Cwd string `json:"cwd,omitempty"`
	// Model may be zero with a Profile, for the router's choice.
	Model    ModelRef `json:"model,omitzero"`
	Thinking string   `json:"thinking,omitempty"`
}

// Role is what an agent does on the team.
type Role string

const (
	RoleSupervisor Role = "supervisor"
	RoleClerk      Role = "clerk"
	RoleAgent      Role = "agent"
)

// Profile is an agent's profile as GET /v1/profiles lists it. A profile
// that breaks a rule lists only its ID and Problem.
type Profile struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Role Role   `json:"role,omitempty"`
	// Model is the profile's model as written; Runs the model a session
	// from it runs, as the router chose it, zero when none can run, with
	// Problem saying why.
	Model ModelRef `json:"model,omitzero"`
	Runs  ModelRef `json:"runs,omitzero"`
	// Home is the folder a session from it starts in by default.
	Home         string   `json:"home,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	Private      bool     `json:"private,omitempty"`
	// Tools are the tools its agent may call; empty for every tool.
	Tools   []string `json:"tools,omitempty"`
	Problem string   `json:"problem,omitempty"`
}

// SendMessage is the body of POST /v1/sessions/{id}/messages.
type SendMessage struct {
	RequestID string `json:"requestId"`
	Text      string `json:"text"`
}

// SentMessage is the answer to SendMessage.
type SentMessage struct {
	SubmissionID int64 `json:"submissionId"`
}

// Abort is the body of POST /v1/sessions/{id}/abort.
type Abort struct {
	RequestID string `json:"requestId"`
}

// Decide is the body of POST /v1/approvals/{id}.
type Decide struct {
	RequestID string `json:"requestId"`
	Allow     bool   `json:"allow"`
	Reason    string `json:"reason,omitempty"`
}

// SessionDetail is the answer to GET /v1/sessions/{id}.
type SessionDetail struct {
	Session Session `json:"session"`
	Entries []Entry `json:"entries"`
	Live    Live    `json:"live"`
}

// ErrorCode names why a request failed.
type ErrorCode string

const (
	ErrorCodeProtocol     ErrorCode = "protocol"
	ErrorCodeUnauthorized ErrorCode = "unauthorized"
	ErrorCodeBadRequest   ErrorCode = "bad_request"
	ErrorCodeNotFound     ErrorCode = "not_found"
	ErrorCodeConflict     ErrorCode = "conflict"
	ErrorCodeInternal     ErrorCode = "internal"
)

// Error is the body of every non-2xx answer.
type Error struct {
	Code    ErrorCode `json:"code"`
	Message string    `json:"message"`
	// Status is the HTTP status the error came with; it is not on the wire.
	Status int `json:"-"`
}

func (e *Error) Error() string { return string(e.Code) + ": " + e.Message }
