package team

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The question tool (ADR-0048): a supervisor's or a private agent's
// session puts a question to the owner inline, in the session the owner
// talks in, and its tool call waits for the answer, which is its result.
// It is no ask: an ask is a thread's specialist's, goes to the Inbox and
// is answered later by a message; a question belongs to its session and
// is answered in the same run. Who may ask, when, how long it waits and
// what an answer holds are the rules here.

// Question is a question call: the question, the answers it suggests,
// whether the owner may pick more than one, and the hint for a free
// answer.
type Question struct {
	Question    string   `json:"question"`
	Options     []string `json:"options,omitempty"`
	Multiple    bool     `json:"multiple,omitempty"`
	Placeholder string   `json:"placeholder,omitempty"`
}

// DefaultQuestionTimeout is how long a question waits for the owner when
// its agent's profile names no questionMinutes: then it is skipped, with
// the reason proto.SkippedTimeout, and the agent goes on.
const DefaultQuestionTimeout = 30 * time.Minute

// QuestionTimeout is how long a question of a session of p waits: p's
// questionMinutes, else DefaultQuestionTimeout.
func QuestionTimeout(p profiles.Profile) time.Duration {
	if p.QuestionMinutes > 0 {
		return time.Duration(p.QuestionMinutes) * time.Minute
	}
	return DefaultQuestionTimeout
}

// CheckQuestion refuses a question call that breaks a rule, and is the
// call as pocketd keeps it, trimmed: a question of 1 to proto.MaxQuestion
// characters; no options, or 2 to proto.MaxQuestionOptions, each of 1 to
// proto.MaxQuestionOption characters, none twice; multiple only with
// options; a placeholder of at most proto.MaxQuestionPlaceholder.
func CheckQuestion(q Question) (Question, error) {
	out := Question{Question: strings.TrimSpace(q.Question), Multiple: q.Multiple, Placeholder: strings.TrimSpace(q.Placeholder)}
	if out.Question == "" {
		return Question{}, errors.New("the question is empty")
	}
	if n := utf8.RuneCountInString(out.Question); n > proto.MaxQuestion {
		return Question{}, fmt.Errorf("the question has %d characters, more than %d", n, proto.MaxQuestion)
	}
	if len(q.Options) != 0 && (len(q.Options) < 2 || len(q.Options) > proto.MaxQuestionOptions) {
		return Question{}, fmt.Errorf("%d options: give none, or 2 to %d", len(q.Options), proto.MaxQuestionOptions)
	}
	for _, o := range q.Options {
		o = strings.TrimSpace(o)
		if o == "" || utf8.RuneCountInString(o) > proto.MaxQuestionOption {
			return Question{}, fmt.Errorf("an option is empty or over %d characters", proto.MaxQuestionOption)
		}
		if slices.Contains(out.Options, o) {
			return Question{}, fmt.Errorf("the option %q is given twice", o)
		}
		out.Options = append(out.Options, o)
	}
	if out.Multiple && len(out.Options) == 0 {
		return Question{}, errors.New("multiple lets the owner pick more than one option, and the question gives none")
	}
	if n := utf8.RuneCountInString(out.Placeholder); n > proto.MaxQuestionPlaceholder {
		return Question{}, fmt.Errorf("the placeholder has %d characters, more than %d", n, proto.MaxQuestionPlaceholder)
	}
	return out, nil
}

// SessionRole is what a session works as, besides its profile: a
// thread's specialist (a plan's lead included), a cross-review, a
// thread's worker, a consult or a gather's proposal session. Plan says a
// thread's specialist is its plan's lead, which plans and builds nothing
// (ADR-0020); it comes with Thread.
type SessionRole struct {
	Thread, Review, Worker, Consult, Gather, Plan bool
}

// none says the session works as none of them.
func (r SessionRole) none() bool { return r == SessionRole{} }

// MayQuestion says whether a session of profile p (nil for none), working
// as role, gets the question tool: a supervisor's or a private agent's
// session, working as none of the roles, which are the sessions the owner
// talks in. Every other session puts its questions as asks, or not at all.
func MayQuestion(p *profiles.Profile, role SessionRole) bool {
	return p != nil && (p.Role == profiles.RoleSupervisor || p.Private) && role.none()
}

// CheckQuestionCaller refuses a question call from a session that does
// not get the tool (MayQuestion), p as its profile is now: one that lists
// it from before is refused too.
func CheckQuestionCaller(p *profiles.Profile, role SessionRole) error {
	if MayQuestion(p, role) {
		return nil
	}
	if role.Thread && !role.Review {
		return errors.New("question is the supervisor's and a private agent's alone: a thread's specialist puts its questions to the owner as asks, in its report with team_message")
	}
	return errors.New("question is the supervisor's and a private agent's alone, in a session the owner talks in: this session does not ask the owner")
}

// ErrNobodyWatching refuses a question in a run the owner's own message
// did not start, as a schedule's, a note's or a thread's report's: no one
// would answer it, and the run would wait out the timeout.
var ErrNobodyWatching = errors.New("nobody is watching this run; ask in prose, in your summary")

// CheckWatched refuses a question in a run unless the owner's own message
// started it (byOwner).
func CheckWatched(byOwner bool) error {
	if byOwner {
		return nil
	}
	return ErrNobodyWatching
}

// ErrQuestionWaits refuses a second question while one waits in the
// session: a session has at most one.
var ErrQuestionWaits = errors.New("a question already waits for the owner in this session: one at a time; wait for its answer, or ask in prose")

// CheckAnswer checks the owner's answer to a question that suggests
// options and takes multiple picks or not (rule 62), and is what the
// question keeps of it: the answer, trimmed, and the options picked, in
// ascending order. Skipped carries no answer and no option. Otherwise an
// answer is 1 to proto.MaxAnswer characters, or the options picked alone,
// each an index of options, none twice, more than one only with multiple;
// with no words, the answer is the options picked, joined by "; ".
func CheckAnswer(options []string, multiple bool, a proto.AnswerQuestion) (answer string, option []int, err error) {
	answer = strings.TrimSpace(a.Answer)
	if a.Skipped {
		if a.Answer != "" || len(a.Option) > 0 {
			return "", nil, errors.New("a skip carries no answer and no option")
		}
		return "", nil, nil
	}
	if n := utf8.RuneCountInString(answer); n > proto.MaxAnswer {
		return "", nil, fmt.Errorf("the answer has %d characters, more than %d", n, proto.MaxAnswer)
	}
	if len(a.Option) > 0 && len(options) == 0 {
		return "", nil, errors.New("the question suggests no option")
	}
	if len(a.Option) > 1 && !multiple {
		return "", nil, errors.New("the question takes one option")
	}
	for _, i := range a.Option {
		if i < 0 || i >= len(options) {
			return "", nil, fmt.Errorf("option %d is not one of the question's %d", i, len(options))
		}
		if slices.Contains(option, i) {
			return "", nil, fmt.Errorf("option %d is picked twice", i)
		}
		option = append(option, i)
	}
	slices.Sort(option)
	if answer == "" {
		if len(option) == 0 {
			return "", nil, fmt.Errorf("an answer is 1 to %d characters, or the options picked", proto.MaxAnswer)
		}
		picked := make([]string, len(option))
		for j, i := range option {
			picked[j] = options[i]
		}
		answer = strings.Join(picked, "; ")
	}
	return answer, option, nil
}

// QuestionResult is the question tool's result for a question that ended,
// as JSON the agent reads: {"answer", "option"} for an answer, "option"
// an index, or a list of them when the question takes multiple picks,
// and absent when no option was picked; {"skipped": true, "reason"} for
// a skip: proto.SkippedByOwner, proto.SkippedTimeout or
// proto.SkippedAborted.
func QuestionResult(q proto.Question) string {
	var out any
	switch {
	case q.State == proto.QuestionSkipped:
		out = struct {
			Skipped bool   `json:"skipped"`
			Reason  string `json:"reason"`
		}{true, q.Reason}
	case len(q.Option) == 0:
		out = struct {
			Answer string `json:"answer"`
		}{q.Answer}
	case q.Multiple:
		out = struct {
			Answer string `json:"answer"`
			Option []int  `json:"option"`
		}{q.Answer, q.Option}
	default:
		out = struct {
			Answer string `json:"answer"`
			Option int    `json:"option"`
		}{q.Answer, q.Option[0]}
	}
	data, err := json.Marshal(out)
	if err != nil {
		panic(err) // fixed shapes of strings and numbers
	}
	return string(data)
}
