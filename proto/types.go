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
	// Images says the model takes images: its catalog entry lists "image"
	// among its inputs, and a message's attached images reach it as
	// images (ADR-0029).
	Images bool `json:"images,omitempty"`
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

// Tokens is the tokens a usage counts: its Total, as the provider reported
// it, else its parts summed when the provider reported none.
func (u Usage) Tokens() int64 {
	if u.Total > 0 {
		return u.Total
	}
	return max(0, u.Input) + max(0, u.Output) + max(0, u.CacheRead) + max(0, u.CacheWrite)
}

// Add returns u plus v. Its Total is the two sides' Tokens: a side whose
// provider reported no total counts its parts, so a sum of usages with and
// without totals loses none of them (ADR-0052; Codex review of #107, round
// 1).
func (u Usage) Add(v Usage) Usage {
	return Usage{
		Input:      u.Input + v.Input,
		Output:     u.Output + v.Output,
		CacheRead:  u.CacheRead + v.CacheRead,
		CacheWrite: u.CacheWrite + v.CacheWrite,
		Total:      u.Tokens() + v.Tokens(),
		Cost:       u.Cost + v.Cost,
	}
}

// UsageDayLayout is how a day of GET /v1/usage is written: its date, in
// the service's time zone (ADR-0052).
const UsageDayLayout = "2006-01-02"

// UsageReport is the answer to GET /v1/usage (ADR-0052, rule 63): the usage
// of the sessions' answers from a day on, summed by day in the service's
// time zone and by model.
type UsageReport struct {
	// Zone is the service's time zone; Today the day it was there when the
	// service answered, and TodayEnds when that day ends: a client asks
	// again then, for the day that begins.
	Zone      string    `json:"zone"`
	Today     string    `json:"today"`
	TodayEnds time.Time `json:"todayEnds"`
	// Since is the first day the report sums, as asked.
	Since string `json:"since"`
	// Days are the days from Since through Today that have usage, oldest
	// first.
	Days []UsageDay `json:"days"`
}

// UsageDay is one day's usage, by model.
type UsageDay struct {
	// Day is the date, as UsageDayLayout writes it.
	Day string `json:"day"`
	// Models are its models' usage, by provider, then by model id.
	Models []ModelUsage `json:"models"`
}

// ModelUsage is one model's usage.
type ModelUsage struct {
	Model ModelRef `json:"model"`
	Usage Usage    `json:"usage"`
}

// SessionState is what a session is doing.
type SessionState string

const (
	// SessionIdle: no run.
	SessionIdle SessionState = "idle"
	// SessionRunning: a run is in progress.
	SessionRunning SessionState = "running"
	// SessionWaiting: a run waits on the owner: an approval, or its
	// question's answer (ADR-0048).
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
	// (ADR-0026, rule 7c): the one New Session opens for that profile.
	Standing bool `json:"standing,omitempty"`
	// StartedBy is the session that started this one, 0 for none: for a
	// thread's worker, the specialist's session that called team_worker
	// (rule 23a). No other session names one.
	StartedBy int64        `json:"startedBy,omitempty"`
	State     SessionState `json:"state"`
	// Question is the question that waits for the owner in this session,
	// nil while none does (ADR-0048, rule 60): its run waits for the
	// answer. A session has at most one.
	Question  *Question `json:"question,omitempty"`
	Usage     Usage     `json:"usage"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
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
	// Images is, on a user entry, how many images the message gave the
	// model (ADR-0029); its text names every attached file.
	Images int `json:"images,omitempty"`
	// Files is, on a user entry, how many files the owner's message
	// attached, as the service sent them apart from its words (ADR-0029):
	// only then does its text end with the files' lines. A list the owner
	// wrote or pasted is words.
	Files int `json:"files,omitempty"`
	// Sender names who sent a user entry that is not the owner's message:
	// a supervisor's (ADR-0032). nil for the owner's, and for every entry
	// the service does not mark.
	Sender *Sender `json:"sender,omitempty"`
}

// SenderKind is who sent a user entry other than the owner.
type SenderKind string

const (
	// SenderSupervisor is a supervisor's message to the specialist of a
	// thread it delegated, sent with team_send (ADR-0032).
	SenderSupervisor SenderKind = "supervisor"
)

// Sender names who sent a user entry or a queued message that is not the
// owner's. The service marks it from its own record of the submission
// that carries the message, never from the message's text.
type Sender struct {
	Kind SenderKind `json:"kind"`
	// Session is the session that sent it: the supervisor's.
	Session int64 `json:"session"`
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
	// Sender is as an Entry's: nil for the owner's message.
	Sender *Sender `json:"sender,omitempty"`
	// Files is as an Entry's: the files the message attached.
	Files int `json:"files,omitempty"`
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

// QuestionState is where a question is (ADR-0048).
type QuestionState string

const (
	// QuestionWaiting: its tool call waits for the owner.
	QuestionWaiting QuestionState = "waiting"
	// QuestionAnswered: the owner answered it; the answer is the call's
	// result.
	QuestionAnswered QuestionState = "answered"
	// QuestionSkipped: it ended with no answer; Reason says why.
	QuestionSkipped QuestionState = "skipped"
)

// Why a question was skipped (Question.Reason).
const (
	// SkippedByOwner: the owner skipped it.
	SkippedByOwner = "owner"
	// SkippedTimeout: no answer came before its Deadline.
	SkippedTimeout = "timeout"
	// SkippedAborted: its run was aborted.
	SkippedAborted = "aborted"
)

// The limits of a question (rule 60). Its answer has at most MaxAnswer
// characters.
const (
	// MaxQuestion is the most characters a question has.
	MaxQuestion = 500
	// MaxQuestionOptions is the most options a question suggests; one
	// that suggests any suggests at least 2.
	MaxQuestionOptions = 6
	// MaxQuestionOption is the most characters an option has.
	MaxQuestionOption = 60
	// MaxQuestionPlaceholder is the most characters of the hint for a
	// free answer.
	MaxQuestionPlaceholder = 120
)

// Question is a question a supervisor's or a private agent's session puts
// to the owner with its question tool, inline in the session
// (ADR-0048): its tool call waits for the answer, which is the call's
// result. It is no ask: it belongs to its session, not to a thread, and
// GET /v1/asks never lists one. A waiting one is its session's Question.
type Question struct {
	// ID is derived from the task and the tool call: a call that waited
	// when the service stopped shows the same question when its task runs
	// again (rule 61).
	ID        string `json:"id"`
	SessionID int64  `json:"sessionId"`
	CallID    string `json:"callId"`
	Question  string `json:"question"`
	// Options are the answers the agent suggests: none, or 2 to 6. Any
	// answer is taken. Multiple lets the owner pick more than one.
	Options  []string `json:"options,omitempty"`
	Multiple bool     `json:"multiple,omitempty"`
	// Placeholder is the hint for a free answer, "" for none.
	Placeholder string        `json:"placeholder,omitempty"`
	State       QuestionState `json:"state"`
	CreatedAt   time.Time     `json:"createdAt"`
	// Deadline is when a question that still waits is skipped, with the
	// reason SkippedTimeout.
	Deadline time.Time `json:"deadline"`
	// Answer is the owner's answer, and Option the indexes of the options
	// picked, from 0, in ascending order: set once answered.
	Answer string `json:"answer,omitempty"`
	Option []int  `json:"option,omitempty"`
	// Reason is why a skipped question was skipped: SkippedByOwner,
	// SkippedTimeout or SkippedAborted.
	Reason string `json:"reason,omitempty"`
	// SettledBy is the device that answered or skipped it, or pocketd.
	SettledBy string     `json:"settledBy,omitempty"`
	SettledAt *time.Time `json:"settledAt,omitempty"`
}

// AnswerQuestion is the body of POST /v1/sessions/{id}/question/{qid}
// (rule 62): an answer, the options picked, or both; or Skipped alone.
type AnswerQuestion struct {
	RequestID string `json:"requestId"`
	Answer    string `json:"answer,omitempty"`
	Option    []int  `json:"option,omitempty"`
	Skipped   bool   `json:"skipped,omitempty"`
}

// CreateSession is the body of POST /v1/sessions.
type CreateSession struct {
	RequestID string `json:"requestId"`
	Title     string `json:"title,omitempty"`
	// Profile starts the session as an agent (ADR-0013): its identity, its
	// tools, and by default its home as Cwd and its model as Model. Empty
	// for a plain session.
	Profile string `json:"profile,omitempty"`
	// Cwd may be empty with a Profile, for its home. With a Repository it
	// is the repository's checkout, or empty for it (rule 6a).
	Cwd string `json:"cwd,omitempty"`
	// Repository is the repository the session works on, owner/name as GET
	// /v1/profiles lists it (rule 6a, ADR-0027): one the Profile names, or,
	// with no profile, one any profile names. The session works in its
	// checkout's folder; with no profile, the profiles may name the
	// repository in more than one folder, and Cwd names which.
	Repository string `json:"repository,omitempty"`
	// Worktree starts the session in a new git worktree of Repository's
	// checkout, the one SessionWorktree names from this request's Title
	// and RequestID: a retry with the same RequestID finishes the same one.
	Worktree bool `json:"worktree,omitempty"`
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
	Home string `json:"home,omitempty"`
	Cwd  string `json:"cwd,omitempty"`
	// Repositories are the repositories its agent works on, by name, each
	// with its checkout's folder (ADR-0027).
	Repositories []Repository `json:"repositories,omitempty"`
	Private      bool         `json:"private,omitempty"`
	// Tools are the tools its agent may call: absent (nil) for every tool,
	// [] for none.
	Tools []string `json:"tools,omitzero"`
	// Standing says the agent keeps one standing session, which New
	// Session opens (rule 7c); CompactAt is the context, in tokens, at
	// which pocketd compacts it, 0 for none of its own.
	Standing  bool `json:"standing,omitempty"`
	CompactAt int  `json:"compactAt,omitempty"`
	// QuestionMinutes is how long a question of its sessions waits for
	// the owner (ADR-0048, rule 60), 0 for the default of 30.
	QuestionMinutes int    `json:"questionMinutes,omitempty"`
	Problem         string `json:"problem,omitempty"`
}

// Repository is a repository a profile names: owner/name, and the folder
// of its checkout on the service host, where a session on it works
// (ADR-0027).
type Repository struct {
	Name   string `json:"name"`
	Folder string `json:"folder"`
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

// AskState is where an ask is. A withdrawn ask waits for no answer: a
// supervisor's message reached its blocked thread first (ADR-0037).
type AskState string

const (
	AskOpen      AskState = "open"
	AskAnswered  AskState = "answered"
	AskWithdrawn AskState = "withdrawn"
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
// Attachments are the ids of uploads of the session (Upload.ID) the
// message carries, in order (ADR-0029); with one, Text may be empty.
type SendMessage struct {
	RequestID   string  `json:"requestId"`
	Text        string  `json:"text"`
	Attachments []int64 `json:"attachments,omitempty"`
}

// Upload is a file the owner uploaded to a session, the answer to POST
// /v1/sessions/{id}/uploads (rule 10a): its name made safe, its type, its
// size in bytes, and where it is on the service host, the path a message
// names it by.
type Upload struct {
	ID        int64  `json:"id"`
	SessionID int64  `json:"sessionId"`
	Name      string `json:"name"`
	MimeType  string `json:"mimeType"`
	Size      int64  `json:"size"`
	Path      string `json:"path"`
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

// ChangeModel is the body of POST /v1/sessions/{id}/model: the owner's
// model for the session, its thinking level, or both (rule 7e). A nil
// Model keeps the session's model; an empty Thinking keeps its level when
// the model takes it, else the model's default.
type ChangeModel struct {
	RequestID string    `json:"requestId"`
	Model     *ModelRef `json:"model,omitempty"`
	Thinking  string    `json:"thinking,omitempty"`
}

// Decide is the body of POST /v1/approvals/{id}.
type Decide struct {
	RequestID string `json:"requestId"`
	Allow     bool   `json:"allow"`
	Reason    string `json:"reason,omitempty"`
}

// Terminal is a shell pocketd runs for the owner on the service host: a
// login shell of the service user, on a pseudo-terminal (ADR-0025).
type Terminal struct {
	ID string `json:"id"`
	// Title is the name of the folder the shell started in; the shell
	// sets its own title in its output.
	Title     string    `json:"title"`
	Cwd       string    `json:"cwd"`
	StartedAt time.Time `json:"startedAt"`
	// Exited is set once the shell ended, with its exit code: 128 and the
	// signal for one a signal ended. A shell stays listed for 60 seconds
	// after it exits.
	Exited   bool `json:"exited,omitempty"`
	ExitCode *int `json:"exitCode,omitempty"`
}

// TerminalFolder says where a new shell starts.
type TerminalFolder string

const (
	// TerminalHome: the service user's home folder; "" is home too.
	TerminalHome TerminalFolder = "home"
	// TerminalSession: the folder of StartTerminal.Session.
	TerminalSession TerminalFolder = "session"
	// TerminalPath: StartTerminal.Path, an absolute path to a folder.
	TerminalPath TerminalFolder = "path"
)

// StartTerminal is the body of POST /v1/terminals.
type StartTerminal struct {
	RequestID string         `json:"requestId"`
	Folder    TerminalFolder `json:"folder,omitempty"`
	Session   int64          `json:"session,omitempty"`
	Path      string         `json:"path,omitempty"`
	// Rows and Cols are the shell's size: 2 to 500 rows and 2 to 1000
	// columns, 0 for 24 by 80.
	Rows int `json:"rows,omitempty"`
	Cols int `json:"cols,omitempty"`
}

// TerminalStarted is the answer to POST /v1/terminals.
type TerminalStarted struct {
	ID string `json:"id"`
}

// TerminalOutput is the answer to GET /v1/terminals/{id}/output: the
// shell's output from byte From to byte To, counted from the shell's
// start. A client asks next from To. Exited says the shell ended, with its
// ExitCode, and no output follows Data.
type TerminalOutput struct {
	Data     []byte `json:"data"`
	From     int64  `json:"from"`
	To       int64  `json:"to"`
	Exited   bool   `json:"exited,omitempty"`
	ExitCode *int   `json:"exitCode,omitempty"`
}

// TerminalInput is the body of POST /v1/terminals/{id}/input: keys for
// the shell, at most MaxTerminalInput bytes. Tab names the client's
// connection to the shell, and Seq numbers its inputs from 1: an input
// whose Seq is not above the last one its tab sent is taken already and
// written no second time.
type TerminalInput struct {
	Tab  string `json:"tab"`
	Seq  int64  `json:"seq"`
	Data []byte `json:"data"`
}

// ResizeTerminal is the body of POST /v1/terminals/{id}/resize.
type ResizeTerminal struct {
	Rows int `json:"rows"`
	Cols int `json:"cols"`
}

// Schedules is the answer to GET /v1/schedules and the payload of a
// schedules event (ADR-0030): the owner's schedules, from the schedules
// file in the profile folder, in the file's order, then each supervisor's
// own that still go out, oldest first.
type Schedules struct {
	// Zone is the service's time zone: a schedule that names none reads
	// its clock times in it.
	Zone string `json:"zone"`
	// Problem says why none of the owner's schedules file goes out, ""
	// when it was read or there is none.
	Problem   string     `json:"problem,omitempty"`
	Schedules []Schedule `json:"schedules"`
}

// ScheduleSource says whose a schedule is.
type ScheduleSource string

const (
	// ScheduleOwner is one of the owner's, from their schedules file: it
	// goes to its profile's standing session.
	ScheduleOwner ScheduleSource = "owner"
	// ScheduleSession is a supervisor's own, from team_schedule: it goes
	// to the session that made it.
	ScheduleSession ScheduleSource = "session"
)

// Schedule is a message that goes to a session later, or on repeat, as
// "[scheduled] <message>" (ADR-0030).
type Schedule struct {
	// ID is the owner's id in their file, or S<n> for a supervisor's own.
	ID     string         `json:"id"`
	Source ScheduleSource `json:"source"`
	// Profile is the agent whose standing session one of the owner's goes
	// to.
	Profile string `json:"profile,omitempty"`
	// Session is the session it goes to: a supervisor's own session, or
	// the profile's standing session; absent while it has none.
	Session int64 `json:"session,omitempty"`
	// When is when it goes out, as written; Repeat says how it repeats,
	// absent for one that goes out once; Zone is the time zone of its
	// clock times.
	When   string `json:"when"`
	Repeat string `json:"repeat,omitempty"`
	Zone   string `json:"zone"`
	// Message is the text it sends; Gather the gather it starts in its
	// place, absent for a message (ADR-0033).
	Message string          `json:"message"`
	Gather  *ScheduleGather `json:"gather,omitempty"`
	// Next is when it goes out next; absent when it does not.
	Next *time.Time `json:"next,omitempty"`
	// Runs counts the messages it sent, Skipped the times a repeat's last
	// message still waited for the session.
	Runs    int           `json:"runs"`
	Skipped int           `json:"skipped"`
	Last    *ScheduleTime `json:"last,omitempty"`
	// Problem says why it does not go out, absent when it does.
	Problem string `json:"problem,omitempty"`
}

// ScheduleGather is the gather a schedule starts (ADR-0033): the skill
// whose text each asked agent gets, the agents (every one its supervisor
// can delegate to when none), and a note to them.
type ScheduleGather struct {
	Skill  string   `json:"skill"`
	Agents []string `json:"agents,omitempty"`
	Note   string   `json:"note,omitempty"`
}

// ScheduleTime is what a schedule's last time did.
type ScheduleTime struct {
	At      time.Time       `json:"at"`
	Outcome ScheduleOutcome `json:"outcome"`
	Why     string          `json:"why,omitempty"`
}

// ScheduleOutcome is what a schedule's time did.
type ScheduleOutcome string

const (
	// ScheduleSent: its message went to the outbox, which places it.
	ScheduleSent ScheduleOutcome = "sent"
	// ScheduleSkipped: a repeat's last message still waited.
	ScheduleSkipped ScheduleOutcome = "skipped"
	// ScheduleMissed: no session took it, as a profile with no standing
	// session yet.
	ScheduleMissed ScheduleOutcome = "missed"
)

// Gather is a gather (ADR-0033): a supervisor asks agents for proposals,
// each in a read-only proposal session of its own, with a skill's text.
type Gather struct {
	ID int64 `json:"id"`
	// Supervisor is the session that asked, and is told once the
	// proposals are in.
	Supervisor int64  `json:"supervisor"`
	Skill      string `json:"skill"`
	Note       string `json:"note,omitempty"`
	// Schedule is the owner's schedule that started it, absent for the
	// supervisor's own.
	Schedule string      `json:"schedule,omitempty"`
	State    GatherState `json:"state"`
	// Deadline is when the asked agents' time ends.
	Deadline  time.Time   `json:"deadline"`
	CreatedAt time.Time   `json:"createdAt"`
	EndedAt   *time.Time  `json:"endedAt,omitempty"`
	Asks      []GatherAsk `json:"asks"`
}

// GatherState is a gather's state: running until every asked agent is
// done, failed or out of time.
type GatherState string

const (
	GatherRunning GatherState = "running"
	GatherDone    GatherState = "done"
)

// GatherAsk is one agent a gather asks.
type GatherAsk struct {
	Agent string         `json:"agent"`
	State GatherAskState `json:"state"`
	// Session is its proposal session, absent while it starts.
	Session int64  `json:"session,omitempty"`
	Why     string `json:"why,omitempty"`
	// Proposals counts its proposals not dropped.
	Proposals int `json:"proposals"`
}

// GatherAskState is an asked agent's state.
type GatherAskState string

const (
	GatherAskStarting GatherAskState = "starting"
	GatherAskRunning  GatherAskState = "running"
	GatherAskDone     GatherAskState = "done"
	GatherAskFailed   GatherAskState = "failed"
)

// Proposal is a row of the board (ADR-0033): work an agent proposed in a
// gather, which the owner picks from and launches as a shift.
type Proposal struct {
	ID int64 `json:"id"`
	// Gather is the gather it came from, and GatheredAt when that gather
	// started.
	Gather     int64     `json:"gather"`
	GatheredAt time.Time `json:"gatheredAt"`
	Agent      string    `json:"agent"`
	Repository string    `json:"repository,omitempty"`
	Title      string    `json:"title"`
	Task       string    `json:"task"`
	StopWhen   string    `json:"stopWhen"`
	MaxRounds  int       `json:"maxRounds"`
	// CI says its shift's done also needs CI green on the thread's pull
	// requests, as a shift's CI does (ADR-0047).
	CI bool `json:"ci,omitempty"`
	// Kind is the gather's skill.
	Kind string `json:"kind"`
	Note string `json:"note,omitempty"`
	// Order is its place on the board, lowest first; Rev moves with each
	// change of its text.
	Order int           `json:"order"`
	Rev   int           `json:"rev"`
	State ProposalState `json:"state"`
	// By is the agent that proposed it.
	By string `json:"by"`
	// Launch is the launch that took it, and Thread the thread it started
	// as, once it did (ADR-0034); both absent for one not launched.
	Launch    int64     `json:"launch,omitempty"`
	Thread    int64     `json:"thread,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// ProposalState is a proposal's state.
type ProposalState string

const (
	ProposalProposed ProposalState = "proposed"
	ProposalDropped  ProposalState = "dropped"
	// ProposalLaunched is a proposal a launch took: it waits to start, or
	// started as its thread (ADR-0034).
	ProposalLaunched ProposalState = "launched"
)

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

// DropProposal is the owner's drop of a proposal at the rev the board
// shows (ADR-0034).
type DropProposal struct {
	RequestID string `json:"requestId"`
	Rev       int    `json:"rev"`
}

// CreateLaunch is the owner's launch of proposals (ADR-0034): their ids,
// in the order they start, and the absolute time their threads end.
type CreateLaunch struct {
	RequestID string    `json:"requestId"`
	Proposals []int64   `json:"proposals"`
	End       time.Time `json:"end"`
}

// Launch is a launch of picked proposals (ADR-0034): each item starts as a
// shift thread delegated from the supervisor's standing session, until
// End.
type Launch struct {
	ID int64 `json:"id"`
	// Supervisor is the standing session the threads are delegated from,
	// and Profile its profile, whose board the proposals were on.
	Supervisor int64       `json:"supervisor"`
	Profile    string      `json:"profile"`
	End        time.Time   `json:"end"`
	State      LaunchState `json:"state"`
	// By is who launched it: owner, or the supervisor's session.
	By        LaunchBy     `json:"by"`
	Items     []LaunchItem `json:"items"`
	CreatedAt time.Time    `json:"createdAt"`
	EndedAt   *time.Time   `json:"endedAt,omitempty"`
}

// LaunchState is a launch's state: running while an item waits.
type LaunchState string

const (
	LaunchRunning LaunchState = "running"
	LaunchDone    LaunchState = "done"
)

// LaunchBy is who launched a launch.
type LaunchBy string

const (
	LaunchByOwner      LaunchBy = "owner"
	LaunchBySupervisor LaunchBy = "supervisor"
)

// LaunchItem is one proposal of a launch.
type LaunchItem struct {
	Proposal int64           `json:"proposal"`
	Agent    string          `json:"agent"`
	Title    string          `json:"title"`
	State    LaunchItemState `json:"state"`
	// Thread is the thread it started as, once started.
	Thread int64 `json:"thread,omitempty"`
	// Why says why it was skipped.
	Why string `json:"why,omitempty"`
}

// LaunchItemState is a launch item's state.
type LaunchItemState string

const (
	LaunchItemWaiting LaunchItemState = "waiting"
	LaunchItemStarted LaunchItemState = "started"
	LaunchItemSkipped LaunchItemState = "skipped"
)
