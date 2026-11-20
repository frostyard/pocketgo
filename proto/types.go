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
	// ContextWindow is how many tokens of context the model takes, as its
	// catalog entry says; 0 when it says none.
	ContextWindow int `json:"contextWindow,omitempty"`
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
	// chose its model: owner, profile, profile-alias, cross-review for a
	// review the service started (rule 46), or worker-asked, worker,
	// worker-team or worker-default for a thread's worker (rule 47): a
	// client treats every route that starts with worker- as a worker's.
	Profile string `json:"profile,omitempty"`
	Route   string `json:"route,omitempty"`
	// Standing says the session is its profile's standing session
	// (ADR-0025, rule 7c): the one New Session opens for that profile.
	Standing  bool         `json:"standing,omitempty"`
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
	// EntryCompaction is a compaction of the session's context: Text is
	// the summary that stands for what came before it (rule 16c).
	EntryCompaction EntryKind = "compaction"
	// EntryReset starts the session's context afresh, as Start Over does:
	// the model sees nothing before it (rule 7d).
	EntryReset EntryKind = "reset"
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
	// Compacting says a compaction of the session's context runs, and
	// CompactionFailure why the newest one failed, until one succeeds
	// (rule 16c).
	Compacting        bool   `json:"compacting,omitempty"`
	CompactionFailure string `json:"compactionFailure,omitempty"`
	// Queued are the messages that wait for a run, oldest first, at most
	// MaxQueuedListed; QueuedMore counts the newer ones it leaves out. A
	// message leaves the queue when a run reaches it and writes its entry,
	// or when an abort of a running session withdraws it; a run that fails
	// keeps it for the next.
	Queued     []QueuedMessage `json:"queued,omitempty"`
	QueuedMore int             `json:"queuedMore,omitempty"`
	// Pending says something waits for a run, a write Queued does not list
	// included, as a compaction's summary that waits for its run's end: a
	// Start Over waits for it too (rule 7d).
	Pending bool `json:"pending,omitempty"`
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
	// Home is its home folder; Cwd the folder a session from it starts in
	// by default: its home, else its workspace.
	Home         string   `json:"home,omitempty"`
	Cwd          string   `json:"cwd,omitempty"`
	Repositories []string `json:"repositories,omitempty"`
	Private      bool     `json:"private,omitempty"`
	// Tools are the tools its agent may call: absent (nil) for every tool,
	// [] for none.
	Tools []string `json:"tools,omitzero"`
	// Standing says the agent keeps one standing session, which New
	// Session opens (rule 7c); CompactAt is the context, in tokens, at
	// which pocketd compacts it, 0 for none of its own.
	Standing  bool   `json:"standing,omitempty"`
	CompactAt int    `json:"compactAt,omitempty"`
	Problem   string `json:"problem,omitempty"`
}

// ThreadState is where a thread is (ADR-0014).
type ThreadState string

const (
	// ThreadStarting: pocketd sets up its folder and session.
	ThreadStarting ThreadState = "starting"
	// ThreadOpen: the specialist works on it.
	ThreadOpen ThreadState = "open"
	// ThreadDone and ThreadBlocked: the specialist's last report said so.
	ThreadDone    ThreadState = "done"
	ThreadBlocked ThreadState = "blocked"
	// ThreadClosed: off the desk.
	ThreadClosed ThreadState = "closed"
)

// Thread is a piece of work a supervisor delegated to a specialist.
type Thread struct {
	ID    int64  `json:"id"`
	Title string `json:"title"`
	// Agent is the specialist's profile id.
	Agent string `json:"agent"`
	// Supervisor is the session that delegated it; Session the
	// specialist's, 0 while the thread starts.
	Supervisor int64 `json:"supervisor"`
	Session    int64 `json:"session,omitempty"`
	// Repository is the repository it works on, "" for the specialist's
	// home; Cwd its folder, and Branch its worktree's branch.
	Repository string      `json:"repository,omitempty"`
	Cwd        string      `json:"cwd,omitempty"`
	Branch     string      `json:"branch,omitempty"`
	Task       string      `json:"task"`
	State      ThreadState `json:"state"`
	// Report is the specialist's last report.
	Report string `json:"report,omitempty"`
	// Plan is the plan the thread works for (ADR-0020), 0 for none;
	// PlanTask the task of it the thread delivers, "" for the plan's own
	// thread, whose specialist plans.
	Plan     int64  `json:"plan,omitempty"`
	PlanTask string `json:"planTask,omitempty"`
	// Shift is the thread's shift (ADR-0024), nil for a thread that runs
	// none.
	Shift     *Shift    `json:"shift,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ShiftState is where a shift is.
type ShiftState string

const (
	// ShiftRunning: pocketd sends its rounds.
	ShiftRunning ShiftState = "running"
	// ShiftSummary: it stopped, and its summary round asks for the
	// specialist's report.
	ShiftSummary ShiftState = "summary"
	// ShiftPaused: the owner wrote to the specialist's session; the owner
	// resumes it.
	ShiftPaused ShiftState = "paused"
	// ShiftEnded: it is over; Reason and Why say why.
	ShiftEnded ShiftState = "ended"
)

// Shift is a thread run in rounds toward its task until its stop condition
// holds or a limit is reached (ADR-0024, rule 55).
type Shift struct {
	Stop      string `json:"stop"`
	MaxRounds int    `json:"maxRounds"`
	// Deadline is nil for none.
	Deadline *time.Time `json:"deadline,omitempty"`
	// CI says done also needs CI green on the thread's pull requests.
	CI    bool       `json:"ci,omitempty"`
	State ShiftState `json:"state"`
	// Reason says why it stopped or paused: done, blocked, max-rounds,
	// deadline, failed-check, failed, person, stopped or closed; Why says
	// it in words.
	Reason string `json:"reason,omitempty"`
	Why    string `json:"why,omitempty"`
	// Round is the round under way, from 1.
	Round int `json:"round"`
	// Failures counts the rounds in a row whose done saw the same checks
	// fail.
	Failures int          `json:"failures,omitempty"`
	Rounds   []ShiftRound `json:"rounds,omitempty"`
	EndedAt  *time.Time   `json:"endedAt,omitempty"`
}

// ShiftRound is a round's verdict: the last STATUS line of the specialist's
// answer (done, continue, blocked, or none).
type ShiftRound struct {
	N       int       `json:"n"`
	Verdict string    `json:"verdict"`
	Reason  string    `json:"reason,omitempty"`
	At      time.Time `json:"at"`
}

// StopShift is the body of POST /v1/threads/{id}/shift/stop, and
// ResumeShift of POST /v1/threads/{id}/shift/resume.
type StopShift struct {
	RequestID string `json:"requestId"`
}

type ResumeShift struct {
	RequestID string `json:"requestId"`
}

// AskState is where an ask is.
type AskState string

const (
	AskOpen     AskState = "open"
	AskAnswered AskState = "answered"
)

// Ask is a decision a thread's specialist needs from the owner.
type Ask struct {
	ID        int64  `json:"id"`
	Thread    int64  `json:"thread"`
	SessionID int64  `json:"sessionId"`
	Question  string `json:"question"`
	// Options are the answers the specialist suggests; any answer is taken.
	Options    []string   `json:"options,omitempty"`
	State      AskState   `json:"state"`
	Answer     string     `json:"answer,omitempty"`
	AnsweredBy string     `json:"answeredBy,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	AnsweredAt *time.Time `json:"answeredAt,omitempty"`
}

// AnswerAsk is the body of POST /v1/asks/{id}.
type AnswerAsk struct {
	RequestID string `json:"requestId"`
	Answer    string `json:"answer"`
}

// CloseThread is the body of POST /v1/threads/{id}/close.
type CloseThread struct {
	RequestID string `json:"requestId"`
}

// LessonState is where a lesson is (ADR-0015).
type LessonState string

const (
	// LessonPending: it waits for the owner, and no session has it.
	LessonPending LessonState = "pending"
	// LessonActive: it is in force, in every session of its agent.
	LessonActive LessonState = "active"
	// LessonDiscarded: the owner, or its writer, dropped it while it
	// waited. LessonForgotten: it was in force, and is no longer.
	LessonDiscarded LessonState = "discarded"
	LessonForgotten LessonState = "forgotten"
)

// LessonChange is a change to a lesson in force that waits for the owner:
// new text, or forgetting it. The lesson stays in force until then.
type LessonChange struct {
	Text   string `json:"text,omitempty"`
	Forget bool   `json:"forget,omitempty"`
	// Writer is the profile of the session that asked for it.
	Writer    string `json:"writer"`
	SessionID int64  `json:"sessionId,omitempty"`
}

// Lesson is something an agent keeps across its sessions, once the owner
// approved it.
type Lesson struct {
	ID int64 `json:"id"`
	// Agent is the profile id of the agent it is for.
	Agent string      `json:"agent"`
	Text  string      `json:"text"`
	State LessonState `json:"state"`
	// Change waits for the owner, on a lesson in force.
	Change *LessonChange `json:"change,omitempty"`
	// Writer is the profile of the session that wrote it.
	Writer    string    `json:"writer"`
	SessionID int64     `json:"sessionId,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Waits says whether the owner has something to decide on l: a new lesson,
// or a change to one in force.
func (l Lesson) Waits() bool {
	return l.State == LessonPending || (l.State == LessonActive && l.Change != nil)
}

// LessonDecision is what the owner decides on a lesson.
type LessonDecision string

const (
	// LessonApprove makes what waits true: a new lesson in force, new text,
	// or forgetting it.
	LessonApprove LessonDecision = "approve"
	// LessonDiscard drops what waits: a new lesson, or a change, while the
	// lesson in force stays.
	LessonDiscard LessonDecision = "discard"
	// LessonForget removes a lesson in force at once.
	LessonForget LessonDecision = "forget"
)

// DecideLesson is the body of POST /v1/lessons/{id}. An approval names
// what the owner saw waiting: its Text, or Forget for a lesson to be
// forgotten. One that no longer waits as seen is refused.
type DecideLesson struct {
	RequestID string         `json:"requestId"`
	Decision  LessonDecision `json:"decision"`
	Text      string         `json:"text,omitempty"`
	Forget    bool           `json:"forget,omitempty"`
}

// PullState is where a linked pull request is on GitHub (ADR-0016).
type PullState string

const (
	PullOpen   PullState = "open"
	PullClosed PullState = "closed"
	PullMerged PullState = "merged"
)

// PullRequest is a pull request a thread's specialist reported, as pocketd
// last read it on GitHub.
type PullRequest struct {
	ID     int64 `json:"id"`
	Thread int64 `json:"thread"`
	// Repository is owner/name; Number its number there.
	Repository string    `json:"repository"`
	Number     int64     `json:"number"`
	Title      string    `json:"title"`
	URL        string    `json:"url"`
	State      PullState `json:"state"`
	Draft      bool      `json:"draft,omitempty"`
	// Head is its branch, HeadSHA that branch's commit when pocketd last
	// read it, and Base the branch it merges into.
	Head    string `json:"head"`
	HeadSHA string `json:"headSha"`
	Base    string `json:"base"`
	// Ready says it has ready-for-human, as GitHub last said or pocketd
	// last wrote; Checks is CI's state at its head when pocketd last read
	// it, "" before; Review the newest cross-review verdict at its head
	// (ADR-0017).
	Ready     bool        `json:"ready,omitempty"`
	Checks    CheckState  `json:"checks,omitempty"`
	Review    *PullReview `json:"review,omitempty"`
	CheckedAt time.Time   `json:"checkedAt"`
	CreatedAt time.Time   `json:"createdAt"`
	UpdatedAt time.Time   `json:"updatedAt"`
}

// CheckState is CI's state at a commit (ADR-0017).
type CheckState string

const (
	ChecksGreen   CheckState = "green"
	ChecksPending CheckState = "pending"
	ChecksFailing CheckState = "failing"
	ChecksNone    CheckState = "none"
)

// ReviewVerdict is a cross-review's verdict.
type ReviewVerdict string

const (
	ReviewReady    ReviewVerdict = "ready"
	ReviewNotReady ReviewVerdict = "not-ready"
	ReviewUnclear  ReviewVerdict = "unclear"
)

// PullReview is a cross-review verdict on a pull request, by a reviewer of
// another model family, at the head it read (ADR-0017).
type PullReview struct {
	ID int64 `json:"id"`
	// Thread is the reviewer's thread, Agent its profile id, and Model and
	// Family its session's model and that model's maker.
	Thread    int64         `json:"thread"`
	Agent     string        `json:"agent"`
	Model     ModelRef      `json:"model"`
	Family    string        `json:"family"`
	Head      string        `json:"head"`
	Verdict   ReviewVerdict `json:"verdict"`
	Summary   string        `json:"summary"`
	Findings  []string      `json:"findings,omitempty"`
	CreatedAt time.Time     `json:"createdAt"`
}

// PlanState is where a plan is (ADR-0020).
type PlanState string

const (
	// PlanDrafting: its lead writes it; asks and consults may be open.
	PlanDrafting PlanState = "drafting"
	// PlanReview: settled, it waits for the owner's approval.
	PlanReview PlanState = "review"
	// PlanApproved: the owner approved one revision and one graph; no
	// task is delivered yet.
	PlanApproved PlanState = "approved"
	// PlanBuilding: a task of it was delivered.
	PlanBuilding PlanState = "building"
	// PlanDone: every task is done, and its thread closed.
	PlanDone PlanState = "done"
	// PlanDropped: its thread closed before it was done.
	PlanDropped PlanState = "dropped"
)

// TaskState is where a plan's task is.
type TaskState string

const (
	// TaskWaiting: a task it depends on is not done.
	TaskWaiting TaskState = "waiting"
	// TaskReady: the supervisor may deliver it.
	TaskReady TaskState = "ready"
	// TaskDelegated: its thread works on it.
	TaskDelegated TaskState = "delegated"
	// TaskDone: its thread closed with its work done.
	TaskDone TaskState = "done"
)

// PlanTask is one task of a plan's graph: one pull request's worth of work
// for one agent.
type PlanTask struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	Agent string `json:"agent"`
	// Repository is the repository it works in, "" for its agent's home.
	Repository string `json:"repository,omitempty"`
	// Acceptance are the checks its work must pass.
	Acceptance []string  `json:"acceptance"`
	Deps       []string  `json:"deps,omitempty"`
	State      TaskState `json:"state"`
	// Thread is the thread of its newest delivery, 0 before one.
	Thread int64 `json:"thread,omitempty"`
}

// PlanGraph is a plan's newest task graph: a draft, or the one the owner
// approved.
type PlanGraph struct {
	Version int `json:"version"`
	// Rev is the plan revision it was drafted for.
	Rev      string     `json:"rev"`
	Approved bool       `json:"approved,omitempty"`
	Tasks    []PlanTask `json:"tasks"`
}

// ConsultState is where a consult is.
type ConsultState string

const (
	ConsultStarting ConsultState = "starting"
	ConsultRunning  ConsultState = "running"
	ConsultAnswered ConsultState = "answered"
	ConsultFailed   ConsultState = "failed"
	ConsultStopped  ConsultState = "stopped"
)

// Consult is an expert's read-only session on a plan, and its one finding.
type Consult struct {
	ID       int64  `json:"id"`
	Agent    string `json:"agent"`
	Question string `json:"question"`
	// Ask is the plan's ask it is on, 0 for none.
	Ask     int64        `json:"ask,omitempty"`
	State   ConsultState `json:"state"`
	Session int64        `json:"session,omitempty"`
	// Stance (supports, objects or neutral) and Summary are its finding.
	Stance  string `json:"stance,omitempty"`
	Summary string `json:"summary,omitempty"`
	// Why says how it ended without a finding.
	Why       string     `json:"why,omitempty"`
	CreatedAt time.Time  `json:"createdAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
}

// Plan is a plan of the team's (ADR-0020): its lead writes it in its plan
// thread, the owner approves one revision of it with its task graph, and
// the supervisor delivers its tasks as threads.
type Plan struct {
	ID int64 `json:"id"`
	// Thread is the plan thread; Supervisor the session that started it;
	// Lead the profile id of the agent that plans.
	Thread     int64     `json:"thread"`
	Supervisor int64     `json:"supervisor"`
	Lead       string    `json:"lead"`
	Title      string    `json:"title"`
	State      PlanState `json:"state"`
	Text       string    `json:"text"`
	// Rev is Text's revision, its SHA-256 in hex; "" before the first
	// write.
	Rev string `json:"rev,omitempty"`
	// Waiting is what the plan's gate waits for, as "A3 open", "A4 not
	// written" or "K2 running"; empty once settled.
	Waiting    []string   `json:"waiting,omitempty"`
	Graph      *PlanGraph `json:"graph,omitempty"`
	Consults   []Consult  `json:"consults,omitempty"`
	ApprovedBy string     `json:"approvedBy,omitempty"`
	ApprovedAt *time.Time `json:"approvedAt,omitempty"`
	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
}

// ApprovePlan is the body of POST /v1/plans/{id}/approve. It names what
// the owner saw: the plan's Rev and its graph's Version. An approval of a
// plan that no longer waits as seen is refused.
type ApprovePlan struct {
	RequestID string `json:"requestId"`
	Rev       string `json:"rev"`
	Graph     int    `json:"graph"`
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

// StartOver is the body of POST /v1/sessions/{id}/start-over: a standing
// session's context starts afresh, in the same session (rule 7d).
type StartOver struct {
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
