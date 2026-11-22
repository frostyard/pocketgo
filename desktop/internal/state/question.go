package state

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// A question is what a supervisor's or a private agent's session puts to
// the owner with its question tool, inline in the session (ADR-0047): its
// run waits for the answer, which is the tool call's result. It is no
// ask. It belongs to its session (proto.Session.Question), never to the
// Inbox's asks (Data.Asks holds the asks alone): it shows in its session's
// transcript (BuildRows' RowQuestion), the session's row says it waits
// with the badge QuestionBadge, and the Inbox has a line for it that opens
// its session (Data.QuestionsWaiting). While it waits, the session's
// composer answers it (Data.ComposerOf). A question has no number in the
// owner's view.

// QuestionBadge is the badge of the row of a session whose question waits
// for the owner, when no count of approvals shows there.
const QuestionBadge = "question"

// The composer's hint while its text answers a question: a desktop's
// names the keys, a compact window's is short (ADR-0044).
const (
	AnswerHint        = "Answer the question. Enter sends, Shift+Enter starts a new line."
	AnswerHintCompact = "Answer the question"
)

// WaitingQuestion is session s's question that waits for the owner, and
// whether one does: a question its summary carries that is not settled.
func WaitingQuestion(s proto.Session) (proto.Question, bool) {
	q := s.Question
	if q == nil || (q.State != "" && q.State != proto.QuestionWaiting) {
		return proto.Question{}, false
	}
	return *q, true
}

// Question is session id's question that waits for the owner, and whether
// one does.
func (d *Data) Question(id int64) (proto.Question, bool) {
	s, ok := d.Session(id)
	if !ok {
		return proto.Question{}, false
	}
	return WaitingQuestion(s)
}

// questionByID is the waiting question with id, of any session.
func (d *Data) questionByID(id string) (proto.Question, bool) {
	for _, s := range d.sessions {
		if q, ok := WaitingQuestion(s); ok && q.ID == id {
			return q, true
		}
	}
	return proto.Question{}, false
}

// QuestionsWaiting are the questions that wait for the owner, one for
// each session that has one, oldest first: the Inbox's lines that open
// their sessions. The Inbox's asks never hold one.
func (d *Data) QuestionsWaiting() []proto.Question {
	var out []proto.Question
	for _, s := range d.sessions {
		if q, ok := WaitingQuestion(s); ok {
			out = append(out, q)
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].SessionID < out[j].SessionID
	})
	return out
}

// QuestionSettled applies the answer of POST
// /v1/sessions/{id}/question/{qid}, as its session's event would: the
// question settled leaves its session, which waits for it no more, and
// the run goes on. A session that waits for another question, or for none,
// is left as it is. A fetch begun before loses to it, as to an event.
func (d *Data) QuestionSettled(q proto.Question) {
	if q.State == "" || q.State == proto.QuestionWaiting {
		return
	}
	s, ok := d.Session(q.SessionID)
	if !ok {
		return
	}
	if cur, waits := WaitingQuestion(s); !waits || cur.ID != q.ID {
		return
	}
	d.rev++
	s.Question = nil
	if s.State == proto.SessionWaiting && len(d.ApprovalsFor(s.ID)) == 0 {
		s.State = proto.SessionRunning
	}
	d.touchSession(s.ID)
	d.putSession(s)
}

// sameQuestion says whether a and b are the same question in the same
// state, as a session's view shows it: nil for none.
func sameQuestion(a, b *proto.Question) bool {
	switch {
	case a == nil || b == nil:
		return a == b
	}
	return a.ID == b.ID && a.State == b.State
}

// QuestionWaits is what a session's row says its question waits for: the
// question's first line, cut short.
func QuestionWaits(q proto.Question) string {
	line, _, more := strings.Cut(strings.TrimSpace(q.Question), "\n")
	return "question: " + Truncate(strings.TrimSpace(line), 80, more)
}

// Composer is what a session's composer does with the owner's text: send
// it as a message, or, while the session's question waits, answer the
// question with it (the owner's decision, ADR-0047).
type Composer struct {
	// Question is the question the text answers, nil when the text is a
	// message.
	Question *proto.Question
}

// ComposerOf is what session id's composer does now: it answers the
// session's question while one waits, and sends a message otherwise.
func (d *Data) ComposerOf(id int64) Composer {
	if q, ok := d.Question(id); ok {
		return Composer{Question: &q}
	}
	return Composer{}
}

// Answers says whether the composer's text answers a question.
func (c Composer) Answers() bool { return c.Question != nil }

// Hint is the composer's placeholder while it answers a question, for a
// compact window or a desktop's; "" while its text is a message, whose
// hints are the window's own. The agent's own hint for a free answer
// (QuestionHint) shows beside the cue that the text answers the question,
// and the cue alone shows without one (Codex review of #95, round 1).
func (c Composer) Hint(compact bool) string {
	if !c.Answers() {
		return ""
	}
	hint := QuestionHint(*c.Question)
	switch {
	case hint == "" && compact:
		return AnswerHintCompact
	case hint == "":
		return AnswerHint
	case compact:
		return "Answer: " + hint
	}
	return "Answer the question: " + strings.TrimRight(hint, ".") + ". Enter sends, Shift+Enter starts a new line."
}

// QuestionHint is q's hint for a free answer, as its card and the
// composer show it: its placeholder's first line alone, trimmed, at most
// proto.MaxQuestionPlaceholder characters; "" for none.
func QuestionHint(q proto.Question) string {
	line, _, _ := strings.Cut(strings.TrimSpace(q.Placeholder), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return ""
	}
	return Truncate(line, proto.MaxQuestionPlaceholder, false)
}

// QuestionCue is what q's card says under its options: that the message
// box below takes any other answer, with the agent's hint for one when it
// gave one, and when q is skipped if no answer comes, in now's zone.
func QuestionCue(q proto.Question, now time.Time) string {
	cue := "Type your answer in the message box below"
	if len(q.Options) > 0 {
		cue = "Or type another answer in the message box below"
	}
	if hint := QuestionHint(q); hint != "" {
		cue += ": " + strings.TrimRight(hint, ".")
	}
	cue += "."
	if !q.Deadline.IsZero() {
		cue += " Skipped " + ClockAt(q.Deadline, now) + " if no answer comes."
	}
	return cue
}

// Attaches says whether files may be attached to what the composer sends:
// not to an answer, which carries text alone.
func (c Composer) Attaches() bool { return !c.Answers() }

// ValidateQuestionAnswer checks the owner's answer to q as pocketd would
// (protocol rule 62): a skip alone; or an answer of at most
// proto.MaxAnswer characters once trimmed, the options picked, or both,
// one of them at least. The options picked are q's, by index from 0, each
// once, in ascending order, and only one unless q takes several.
func ValidateQuestionAnswer(q proto.Question, a proto.AnswerQuestion) error {
	text := strings.TrimSpace(a.Answer)
	if a.Skipped {
		if text != "" || len(a.Option) > 0 {
			return errors.New("a skip carries no answer")
		}
		return nil
	}
	if text == "" && len(a.Option) == 0 {
		return errors.New("the answer is empty")
	}
	if n := utf8.RuneCountInString(text); n > proto.MaxAnswer {
		return fmt.Errorf("the answer has %d characters, more than the %d an answer may have", n, proto.MaxAnswer)
	}
	if len(a.Option) > 1 && !q.Multiple {
		return errors.New("this question takes one option")
	}
	for i, o := range a.Option {
		if o < 0 || o >= len(q.Options) {
			return fmt.Errorf("the question has no option %d", o+1)
		}
		if i > 0 && o <= a.Option[i-1] {
			return errors.New("the options picked are not each once, in order")
		}
	}
	return nil
}

// questionKey is the request-id key of the owner's answer to question id.
func questionKey(id string) string { return "question:" + id }

// questionAction is what an answer's request id is kept for: a skip, or
// its text and the options it picked, so another answer is a new request.
// The text's length comes first, so no text and options read as another's.
func questionAction(a proto.AnswerQuestion) string {
	if a.Skipped {
		return "skip"
	}
	action := strconv.Itoa(len(a.Answer)) + ":" + a.Answer
	for _, o := range a.Option {
		action += "," + strconv.Itoa(o)
	}
	return action
}

// QuestionRefusal is the owner's answer to a question that pocketd refused
// because the question no longer waits, as one answered on another device
// first, or words written for a question that settled before they went:
// Why says why, the service's reason when it gave one, and Answer is what
// the owner wrote, so it is not lost with the question's card.
type QuestionRefusal struct {
	Session int64
	Answer  string
	Why     string
}

// QuestionGone is why words written for a question that settled before
// they went were not sent.
const QuestionGone = "the question no longer waits: it was answered on another device, skipped when its time ran out, or its run stopped"

// questionDraft is the owner's answer being written to a question in its
// session's composer.
type questionDraft struct {
	session int64
	text    string
}

// QuestionDraft is the owner's answer to q being written in its session's
// composer, which the field edits in place while q waits. It is q's alone,
// apart from the session's message draft and from any other question's,
// so a settlement or a new question never sends its words elsewhere: once
// q settles, words that did not go with its answer are kept as the
// session's QuestionRefusal, until the owner dismisses them (Codex review
// of #95, round 1).
func (s *Store) QuestionDraft(q proto.Question) *string {
	d := s.questionDrafts[q.ID]
	if d == nil {
		d = &questionDraft{session: q.SessionID}
		s.questionDrafts[q.ID] = d
	}
	return &d.text
}

// keepRefused keeps text, the owner's words for a question of session that
// did not go, with why, beside words kept already.
func (s *Store) keepRefused(session int64, text, why string) {
	r, kept := s.questionRefusals[session]
	switch {
	case !kept:
		r = QuestionRefusal{Session: session, Answer: text, Why: why}
	case r.Answer == "" || r.Answer == text:
		r.Answer = text
	case text != "":
		r.Answer += "\n\n" + text
	}
	s.questionRefusals[session] = r
}

// AnsweringQuestion is where the owner's answer to question id stands.
func (s *Store) AnsweringQuestion(id string) Pending { return valueOf(s.questionAnswers[id]) }

// QuestionRefusal is the refused answer of session id's composer, if one
// is kept: until the owner dismisses it.
func (s *Store) QuestionRefusal(id int64) (QuestionRefusal, bool) {
	r, ok := s.questionRefusals[id]
	return r, ok
}

// DismissQuestionRefusal drops session id's refused answer.
func (s *Store) DismissQuestionRefusal(id int64) { delete(s.questionRefusals, id) }

// AnswerQuestion answers q with text, the owner's words, and the options
// picked (by index, from 0), either of which may be empty; done runs on
// the UI thread with the outcome. The answer's reply takes the question
// off its session (Data.QuestionSettled); one tried again unchanged after
// a failure keeps its request id, so a lost reply is not answered twice.
// An answer to a question settled elsewhere, as on another device (409),
// or gone (404), is refused: the sessions are fetched again, and the
// refusal, with the service's reason and the owner's words, stays in
// QuestionRefusal after the question's card is gone.
//
// The options picked go in ascending order, each once, as pocketd takes
// them, whatever order they were picked in.
func (s *Store) AnswerQuestion(q proto.Question, text string, option []int, done func(error)) {
	picked := slices.Clone(option)
	slices.Sort(picked)
	picked = slices.Compact(picked)
	s.settleQuestion(q, proto.AnswerQuestion{Answer: strings.TrimSpace(text), Option: picked}, text, done)
}

// SkipQuestion skips q: the agent's run goes on with no answer.
func (s *Store) SkipQuestion(q proto.Question) {
	s.settleQuestion(q, proto.AnswerQuestion{Skipped: true}, "", func(error) {})
}

func (s *Store) settleQuestion(q proto.Question, request proto.AnswerQuestion, typed string, done func(error)) {
	if p := s.questionAnswers[q.ID]; p != nil && p.Busy {
		done(errors.New("the last answer is still on its way"))
		return
	}
	// An answer goes to the question it was written for, and only while
	// that one waits: words for a question that settled meanwhile are
	// kept, never sent to the session's next question or as a message
	// (Codex review of #95, round 1).
	if _, waits := s.Data.questionByID(q.ID); !waits {
		if strings.TrimSpace(typed) != "" {
			s.keepRefused(q.SessionID, typed, QuestionGone)
		}
		s.dropDraft(q.ID, typed)
		s.settleQuestions()
		done(errors.New(QuestionGone))
		return
	}
	p := pending(s.questionAnswers, q.ID)
	if err := ValidateQuestionAnswer(q, request); err != nil {
		p.Err = err.Error()
		done(err)
		return
	}
	key := questionKey(q.ID)
	request.RequestID = s.ids.For(key, questionAction(request))
	p.Busy, p.Err = true, ""
	what := "Answer"
	if request.Skipped {
		what = "Skip"
	}
	s.log(LevelCommand, "%s the question of session %d", what, q.SessionID)
	var settled proto.Question
	s.do(fmt.Sprintf("%s the question of session %d", what, q.SessionID), func(ctx context.Context, c *proto.Client) (err error) {
		settled, err = c.AnswerQuestion(ctx, q.SessionID, q.ID, request)
		return err
	}, func(err error) {
		p.Busy = false
		var pe *proto.Error
		switch {
		case err == nil:
			s.ids.Done(key)
			delete(s.questionAnswers, q.ID)
			// The words that went leave the draft; any others stay, and
			// are kept once the question settled (settleQuestions).
			s.dropDraft(q.ID, typed)
			s.Data.QuestionSettled(settled)
		case errors.As(err, &pe) && (pe.Code == proto.ErrorCodeConflict || pe.Code == proto.ErrorCodeNotFound):
			// The service says why: settled on another device first, by
			// its time running out, or by an abort. The card goes with the
			// reload, or went with the session's event; the refusal stays,
			// with the owner's words.
			s.ids.Done(key)
			delete(s.questionAnswers, q.ID)
			why := pe.Message
			if pe.Code == proto.ErrorCodeNotFound || why == "" {
				why = "pocketd no longer has this question."
			}
			s.questionRefusals[q.SessionID] = QuestionRefusal{Session: q.SessionID, Answer: typed, Why: why}
			s.dropDraft(q.ID, typed)
			s.loadSessions()
		default:
			p.Err = err.Error()
		}
		s.settleQuestions()
		done(err)
	})
}

// dropDraft drops question id's draft when it holds typed, the words that
// went or were kept already: words written since stay in it.
func (s *Store) dropDraft(id, typed string) {
	if d := s.questionDrafts[id]; d != nil && strings.TrimSpace(d.text) == strings.TrimSpace(typed) {
		delete(s.questionDrafts, id)
	}
}

// settleQuestions drops the retry state of each answer whose question no
// longer waits: no card offers its retry. One on its way keeps its state.
// The draft of a question that settled with no answer of its words on its
// way, as one answered on another device, skipped when its time ran out,
// or followed by the session's next question, leaves the composer, and its
// words are kept as the session's QuestionRefusal (Codex review of #95,
// round 1); an answer on its way decides its draft when its reply comes,
// so a settlement event ahead of the reply keeps nothing twice.
func (s *Store) settleQuestions() {
	for id, d := range s.questionDrafts {
		if _, waits := s.Data.questionByID(id); waits {
			continue
		}
		if p := s.questionAnswers[id]; p != nil && p.Busy {
			continue
		}
		if strings.TrimSpace(d.text) != "" {
			s.keepRefused(d.session, d.text, QuestionGone)
		}
		delete(s.questionDrafts, id)
	}
	for id, p := range s.questionAnswers {
		if p.Busy {
			continue
		}
		if _, waits := s.Data.questionByID(id); waits {
			continue
		}
		delete(s.questionAnswers, id)
		s.ids.Done(questionKey(id))
	}
}
