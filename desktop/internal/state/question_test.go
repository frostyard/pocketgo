package state

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

// question is a question waiting in sessionID, from its call "q1".
func question(id string, sessionID int64, text string, created time.Duration, options ...string) proto.Question {
	return proto.Question{ID: id, SessionID: sessionID, CallID: "q1", Question: text, Options: options, State: proto.QuestionWaiting,
		CreatedAt: pocketdtest.Now.Add(created), Deadline: pocketdtest.Now.Add(created + 30*time.Minute)}
}

// waitingOn is session id with q waiting in it, as pocketd lists it.
func waitingOn(id int64, q proto.Question) proto.Session {
	s := session(id, "Odrade", 0)
	s.Profile, s.State, s.Question = "odrade", proto.SessionWaiting, &q
	return s
}

// While the session's question waits, its composer answers it, with the
// hint that says so, and attaches no file; once it is settled, the
// composer sends messages again (the owner's decision, ADR-0047).
func TestTheComposerAnswersAWaitingQuestion(t *testing.T) {
	var d Data
	d.SetSessions(d.Begin(), []proto.Session{session(1, "plain", 0), waitingOn(2, question("q-a", 2, "Which database?", 0, "Postgres", "SQLite"))})
	if c := d.ComposerOf(1); c.Answers() || c.Hint(false) != "" || !c.Attaches() {
		t.Errorf("a session with no question: %+v", c)
	}
	c := d.ComposerOf(2)
	if !c.Answers() || c.Question.ID != "q-a" || c.Attaches() {
		t.Fatalf("a session whose question waits: %+v", c)
	}
	if got := c.Hint(false); got != "Answer the question. Enter sends, Shift+Enter starts a new line." {
		t.Errorf("a desktop's hint %q", got)
	}
	if got := c.Hint(true); got != "Answer the question" {
		t.Errorf("a compact window's hint %q", got)
	}
	// A settled question in a summary is none.
	settled := question("q-a", 2, "Which database?", 0)
	settled.State = proto.QuestionAnswered
	d.QuestionSettled(settled)
	if c := d.ComposerOf(2); c.Answers() || !c.Attaches() {
		t.Errorf("after the answer, the composer still answers: %+v", c)
	}
	s, _ := d.Session(2)
	if s.Question != nil || s.State != proto.SessionRunning {
		t.Errorf("the answered session %+v", s)
	}
	stale := waitingOn(3, question("q-b", 3, "Again?", 0))
	stale.Question.State = proto.QuestionSkipped
	if _, waits := WaitingQuestion(stale); waits {
		t.Error("a skipped question reads as waiting")
	}
}

// A question is never an ask: the Inbox's asks never hold it, and it
// counts as a session that waits, with a line of its own
// (QuestionsWaiting), oldest first.
func TestAQuestionIsNeverAnAsk(t *testing.T) {
	var d Data
	d.SetAsks(d.BeginAsks(), nil)
	d.SetLessons(d.BeginLessons(), nil)
	d.SetApprovals(d.Begin(), nil)
	d.SetPlans(d.BeginPlans(), nil)
	d.SetPulls(d.BeginPulls(), nil)
	d.SetSessions(d.Begin(), []proto.Session{waitingOn(2, question("q-new", 2, "Ship it?", time.Minute)), waitingOn(3, question("q-old", 3, "Which day?", 0))})
	if len(d.Asks()) != 0 {
		t.Fatalf("the asks hold a question: %+v", d.Asks())
	}
	got := d.QuestionsWaiting()
	if len(got) != 2 || got[0].ID != "q-old" || got[1].ID != "q-new" {
		t.Fatalf("the questions waiting %+v, want the oldest first", got)
	}
	for _, k := range d.Inbox() {
		switch k.List {
		case "asks":
			if k.Count != 0 {
				t.Errorf("the asks count a question: %+v", k)
			}
		case "sessions":
			if k.Count != 2 || k.Status != ListLoaded {
				t.Errorf("the sessions that wait %+v", k)
			}
		}
	}
	if note := InboxNote(d.Inbox()); note != "2 sessions wait for you" {
		t.Errorf("the Inbox says %q", note)
	}
	if badge := InboxBadge(d.Inbox()); badge != "2" {
		t.Errorf("the Inbox tab's badge %q", badge)
	}
	// An ask event never brings a question, nor takes one.
	a := ask(1, 2, "Squash?", 0)
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &a})
	if len(d.Asks()) != 1 || len(d.QuestionsWaiting()) != 2 {
		t.Errorf("asks %v, questions %+v", askIDs(d.Asks()), d.QuestionsWaiting())
	}
	// The sessions failing to load: the Inbox cannot say none waits.
	d.SetSessionsError(d.Begin(), "timeout")
	if note := InboxNote(d.Inbox()); !strings.Contains(note, "the sessions are not loaded") && !strings.HasPrefix(note, "The sessions are not loaded") {
		t.Errorf("a failed sessions fetch: the Inbox says %q", note)
	}
}

// A session whose question waits waits for the owner, even while its run
// is busy, which it is while the tool call waits; its row has the raised
// hand, the question badge and what it asks. An approval's count wins
// the badge.
func TestAWaitingQuestionMarksItsSessionsRow(t *testing.T) {
	var d Data
	d.SetSessions(d.Begin(), []proto.Session{waitingOn(2, question("q-a", 2, "Which database\nshould the cache use?", 0))})
	d.SetOpen(2)
	d.Apply(proto.Event{Type: proto.EventLive, Live: &proto.Live{SessionID: 2, Busy: true}})
	if st := d.StateOf(2); st != proto.SessionWaiting {
		t.Errorf("the open session's state %v, want waiting", st)
	}
	if w, _ := d.Counts(); w != 1 {
		t.Errorf("%d sessions wait", w)
	}
	r, ok := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Session(2)
	if !ok || r.Mark != MarkWaiting || !r.Question || r.Badge != QuestionBadge || r.Waits != "question: Which database …" {
		t.Errorf("the row %+v", r)
	}
	if g, _ := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Group("odrade"); g.Waiting != 1 {
		t.Errorf("the group %+v", g)
	}
	d.SetApprovals(d.Begin(), []proto.Approval{approval("a1", 2, "b1", 0)})
	if r, _ := d.Sidebar(SidebarOptions{Now: pocketdtest.Now}).Session(2); r.Badge != "1" || !strings.HasPrefix(r.Waits, "bash") {
		t.Errorf("with an approval too, the row %+v", r)
	}
}

// The open session's rows are built again when its question comes or
// goes, which no entry or live event says (ADR-0047).
func TestAQuestionChangeMovesTheOpenSessionsRev(t *testing.T) {
	var d Data
	d.SetSessions(d.Begin(), []proto.Session{session(2, "Odrade", 0)})
	d.SetOpen(2)
	rev := d.Open().Rev()
	waiting := waitingOn(2, question("q-a", 2, "Ship it?", 0))
	d.Apply(proto.Event{Type: proto.EventSession, Session: &waiting})
	if d.Open().Rev() == rev {
		t.Fatal("a question that came did not move the open session's rev")
	}
	rev = d.Open().Rev()
	same := waiting
	same.UpdatedAt = same.UpdatedAt.Add(time.Second)
	d.Apply(proto.Event{Type: proto.EventSession, Session: &same})
	if d.Open().Rev() != rev {
		t.Error("the same question moved the rev")
	}
	d.SetSessions(d.Begin(), []proto.Session{session(2, "Odrade", time.Minute)})
	if d.Open().Rev() == rev {
		t.Error("a list that took the question away did not move the rev")
	}
}

// The waiting question follows its call's row, which waits for the
// owner; no working line shows meanwhile. With no row of its call, it
// ends the run's rows; a settled one shows nothing.
func TestBuildRowsShowsTheWaitingQuestion(t *testing.T) {
	q := question("q-a", 1, "Which database?", 0, "Postgres", "SQLite")
	entries := []proto.Entry{
		{ID: 1, Kind: proto.EntryUser, Text: "Set up the cache."},
		{ID: 2, Kind: proto.EntryAssistant, Text: "One thing first.", ToolCalls: []proto.ToolCall{call("q1", "question", `{"question":"Which database?"}`)}},
	}
	live := proto.Live{Busy: true, Tools: []proto.LiveTool{{CallID: "q1", Name: "question", Status: proto.ToolRunning}}}
	rows := BuildRows(entries, live, nil, &q)
	if got := strings.Join(kinds(rows), " "); got != "user:e1 assistant:e2 tool:cq1 question:question:q-a" {
		t.Fatalf("rows %s", got)
	}
	if rows[2].Tool.State != ToolWaiting || rows[2].Tool.State.String() != "waiting for you" {
		t.Errorf("the question's call %+v", rows[2].Tool)
	}
	if rows[3].Question == nil || rows[3].Question.ID != "q-a" {
		t.Errorf("the question's row %+v", rows[3])
	}
	// Its call not written yet: the question ends the rows.
	rows = BuildRows(entries[:1], proto.Live{Busy: true}, nil, &q)
	if got := strings.Join(kinds(rows), " "); got != "user:e1 question:question:q-a" {
		t.Errorf("with no call: rows %s", got)
	}
	settled := q
	settled.State = proto.QuestionAnswered
	rows = BuildRows(entries, live, nil, &settled)
	if got := strings.Join(kinds(rows), " "); got != "user:e1 assistant:e2 tool:cq1 working:working" {
		t.Errorf("with a settled question: rows %s", got)
	}
}

func TestValidateQuestionAnswer(t *testing.T) {
	one := question("q", 1, "Which?", 0, "a", "b", "c")
	several := one
	several.Multiple = true
	free := question("q", 1, "Why?", 0)
	for _, tc := range []struct {
		name string
		q    proto.Question
		a    proto.AnswerQuestion
		want string
	}{
		{"words", free, proto.AnswerQuestion{Answer: "because"}, ""},
		{"an option", one, proto.AnswerQuestion{Option: []int{2}}, ""},
		{"an option and words", one, proto.AnswerQuestion{Option: []int{0}, Answer: "a, but later"}, ""},
		{"several options", several, proto.AnswerQuestion{Option: []int{0, 2}}, ""},
		{"a skip", one, proto.AnswerQuestion{Skipped: true}, ""},
		{"nothing", one, proto.AnswerQuestion{Answer: "  "}, "empty"},
		{"too long", free, proto.AnswerQuestion{Answer: strings.Repeat("é", proto.MaxAnswer+1)}, "more than"},
		{"a skip with words", one, proto.AnswerQuestion{Skipped: true, Answer: "no"}, "no answer"},
		{"two of one", one, proto.AnswerQuestion{Option: []int{0, 1}}, "one option"},
		{"no such option", one, proto.AnswerQuestion{Option: []int{3}}, "no option 4"},
		{"an option of none", free, proto.AnswerQuestion{Option: []int{0}}, "no option 1"},
		// As pocketd takes them: any order, none twice, and a skip with
		// no answer at all (Codex review of #95, round 2).
		{"out of order", several, proto.AnswerQuestion{Option: []int{2, 0}}, ""},
		{"twice", several, proto.AnswerQuestion{Option: []int{1, 1}}, "picked twice"},
		{"a skip with white space", one, proto.AnswerQuestion{Skipped: true, Answer: "  "}, "no answer"},
	} {
		err := ValidateQuestionAnswer(tc.q, tc.a)
		switch {
		case tc.want == "" && err != nil:
			t.Errorf("%s: %v", tc.name, err)
		case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
			t.Errorf("%s: %v, want %q", tc.name, err, tc.want)
		}
	}
}

// An option's click answers at once: pocketd gets the option alone, and
// the question leaves its session with the answer's reply. The composer's
// words answer the next question, trimmed, with no option.
func TestAnsweringAQuestionTakesItOffItsSession(t *testing.T) {
	h := newHarness(t)
	q := question("q-a", 5, "Which database?", 0, "Postgres", "SQLite")
	h.srv.AddSession(waitingOn(5, q))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return ok })
	var done []error
	h.store.AnswerQuestion(q, "", []int{1}, func(err error) { done = append(done, err) })
	if !h.store.AnsweringQuestion("q-a").Busy {
		t.Error("the answer is not busy while it is on its way")
	}
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return !ok && len(done) == 1 })
	if done[0] != nil {
		t.Fatalf("the answer failed: %v", done[0])
	}
	var body proto.AnswerQuestion
	reqs := h.srv.RequestsTo(http.MethodPost, proto.QuestionPath(5, "q-a"))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Answer != "" || !slices.Equal(body.Option, []int{1}) || body.RequestID == "" {
		t.Fatalf("the answer sent %+v", reqs)
	}
	if got, _ := h.srv.Question("q-a"); got.State != proto.QuestionAnswered || got.Answer != "SQLite" {
		t.Errorf("pocketd's question %+v", got)
	}
	if st := h.store.Data.StateOf(5); st == proto.SessionWaiting {
		t.Errorf("the session still waits: %v", st)
	}
	h.settle(t)
	if _, kept := h.store.questionAnswers["q-a"]; kept {
		t.Error("the answer's state outlived its question")
	}

	// The next question, answered in words with options picked out of
	// order: they go in order, each once.
	next := question("q-b", 5, "Which days?", time.Minute, "mon", "tue", "wed")
	next.Multiple = true
	h.srv.PutQuestion(next)
	waiting := waitingOn(5, next)
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &waiting})
	h.loop.RunUntil(t, func() bool { got, ok := h.store.Data.Question(5); return ok && got.ID == "q-b" })
	h.store.AnswerQuestion(next, "  mornings only ", []int{2, 0, 2}, func(error) {})
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return !ok })
	reqs = h.srv.RequestsTo(http.MethodPost, proto.QuestionPath(5, "q-b"))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Answer != "mornings only" || !slices.Equal(body.Option, []int{0, 2}) {
		t.Fatalf("the second answer sent %+v", reqs)
	}
}

// Skip settles the question with no answer.
func TestSkippingAQuestion(t *testing.T) {
	h := newHarness(t)
	q := question("q-a", 5, "Which database?", 0)
	h.srv.AddSession(waitingOn(5, q))
	h.connect(t)
	h.store.SkipQuestion(q)
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return !ok })
	var body proto.AnswerQuestion
	reqs := h.srv.RequestsTo(http.MethodPost, proto.QuestionPath(5, "q-a"))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || !body.Skipped || body.Answer != "" || len(body.Option) != 0 {
		t.Fatalf("the skip sent %+v", reqs)
	}
	if got, _ := h.srv.Question("q-a"); got.State != proto.QuestionSkipped || got.Reason != proto.SkippedByOwner {
		t.Errorf("pocketd's question %+v", got)
	}
}

// An answer to a question settled elsewhere first (on another device, by
// its time or by an abort) is refused with pocketd's words, kept with the
// owner's own until dismissed, or until the owner sends again; the draft is
// not cleared, since pocketd did not take it.
func TestAnAnswerToASettledQuestionShowsWhy(t *testing.T) {
	h := newHarness(t)
	q := question("q-a", 5, "Which database?", 0)
	h.srv.AddSession(waitingOn(5, q))
	h.connect(t)
	elsewhere := q
	elsewhere.State, elsewhere.Answer, elsewhere.SettledBy = proto.QuestionAnswered, "Postgres", "phone"
	h.srv.PutQuestion(elsewhere)
	var done []error
	h.store.AnswerQuestion(q, " SQLite\n", nil, func(err error) { done = append(done, err) })
	h.loop.RunUntil(t, func() bool { return len(done) == 1 })
	if done[0] == nil {
		t.Fatal("the answer to a settled question succeeded")
	}
	r, ok := onlyRefusal(h.store, 5)
	if !ok || r.Answer != " SQLite\n" || !strings.Contains(r.Why, "answered already, on phone: the first answer stands") || !strings.Contains(r.Why, "phone") {
		t.Fatalf("the refusal %+v, %v", r, ok)
	}
	h.settle(t)
	if _, waits := h.store.Data.Question(5); waits {
		t.Error("the sessions' reload left the settled question waiting")
	}
	if h.store.AnsweringQuestion("q-a") != (Pending{}) {
		t.Errorf("the refused answer keeps state %+v", h.store.AnsweringQuestion("q-a"))
	}
	h.store.DismissQuestionRefusal("q-a")
	if _, ok := onlyRefusal(h.store, 5); ok {
		t.Error("dismissed, it stays")
	}
}

// An answer tried again unchanged after a failure keeps its request id, so
// a lost reply is not answered twice; another answer is a new request.
func TestAQuestionsAnswerTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	q := question("q-a", 5, "Which database?", 0, "Postgres", "SQLite")
	h.srv.AddSession(waitingOn(5, q))
	h.connect(t)
	p := proto.QuestionPath(5, "q-a")
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	h.store.AnswerQuestion(q, "Postgres", nil, func(error) {})
	h.loop.RunUntil(t, func() bool { return h.store.AnsweringQuestion("q-a").Err != "" })
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	h.store.AnswerQuestion(q, "Postgres", nil, func(error) {})
	h.loop.RunUntil(t, func() bool {
		a := h.store.AnsweringQuestion("q-a")
		return !a.Busy && a.Err != "" && len(h.srv.RequestsTo(http.MethodPost, p)) == 2
	})
	h.store.AnswerQuestion(q, "SQLite", nil, func(error) {})
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return !ok })
	got := sentIDs(t, h, p)
	if len(got) != 3 || got[0] != got[1] || got[2] == got[1] {
		t.Errorf("request ids %q, want the retry to keep the first id and another answer to take a new one", got)
	}
}

// The answer's reply wins over a sessions fetch begun before it, which
// still lists the question waiting.
func TestASettledQuestionStaysGoneOverAStaleList(t *testing.T) {
	var d Data
	q := question("q-a", 2, "Ship it?", 0)
	d.SetSessions(d.Begin(), []proto.Session{waitingOn(2, q)})
	f := d.Begin()
	settled := q
	settled.State = proto.QuestionSkipped
	d.QuestionSettled(settled)
	d.SetSessions(f, []proto.Session{waitingOn(2, q)})
	if _, waits := d.Question(2); waits {
		t.Error("a stale list brought the settled question back")
	}
	// Another question in the session is not this answer's to settle.
	other := question("q-b", 2, "And now?", time.Minute)
	d.Apply(proto.Event{Type: proto.EventSession, Session: ptrTo(waitingOn(2, other))})
	d.QuestionSettled(settled)
	if got, waits := d.Question(2); !waits || got.ID != "q-b" {
		t.Errorf("an answer to another question took %+v", got)
	}
}

func ptrTo[T any](v T) *T { return &v }

// onlyRefusal is session id's one kept refusal, and whether exactly one is
// kept.
func onlyRefusal(s *Store, id int64) (QuestionRefusal, bool) {
	all := s.QuestionRefusals(id)
	if len(all) != 1 {
		return QuestionRefusal{}, false
	}
	return all[0], true
}

// An answer pocketd took whose reply was lost is checked again, with the
// same request id, once its question leaves the session: its words are
// not refused (Codex review of #95, round 2). One that never reached
// pocketd, whose question another device answered, is refused with
// pocketd's words.
func TestALostAnswerReplyIsCheckedBeforeItsWordsAreRefused(t *testing.T) {
	t.Run("taken", func(t *testing.T) {
		h := newHarness(t)
		q := question("q-a", 5, "Which goes first?", 0)
		h.srv.AddSession(waitingOn(5, q))
		h.connect(t)
		h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return ok })
		*h.store.QuestionDraft(q) = "Snow first."
		path := proto.QuestionPath(5, "q-a")
		// pocketd takes it, its session's event comes, and the reply is
		// lost.
		h.srv.LoseAnswerOnce(http.MethodPost, path)
		h.store.AnswerQuestion(q, "Snow first.", nil, func(error) {})
		h.loop.RunUntil(t, func() bool { return len(h.srv.RequestsTo(http.MethodPost, path)) == 2 && h.store.InFlight() == 0 })
		h.settle(t)
		if got := sentIDs(t, h, path); len(got) != 2 || got[0] != got[1] {
			t.Errorf("request ids %q, want the check to send the same one", got)
		}
		if all := h.store.QuestionRefusals(5); len(all) != 0 {
			t.Errorf("an answer pocketd took is kept as refused: %+v", all)
		}
		if _, left := h.store.questionDrafts["q-a"]; left {
			t.Error("the answer that went stays in its draft")
		}
		if h.store.CheckingAnswer(5) || len(h.store.questionUnsure) != 0 || h.store.AnsweringQuestion("q-a") != (Pending{}) {
			t.Error("the answer's check left state behind")
		}
	})
	t.Run("answered on another device", func(t *testing.T) {
		h := newHarness(t)
		q := question("q-a", 5, "Which goes first?", 0)
		h.srv.AddSession(waitingOn(5, q))
		h.connect(t)
		h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return ok })
		*h.store.QuestionDraft(q) = "Snow first."
		path := proto.QuestionPath(5, "q-a")
		h.srv.FailOnce(http.MethodPost, path, http.StatusBadGateway)
		h.store.AnswerQuestion(q, "Snow first.", nil, func(error) {})
		h.loop.RunUntil(t, func() bool { return h.store.AnsweringQuestion("q-a").Err != "" })
		if all := h.store.QuestionRefusals(5); len(all) != 0 {
			t.Fatalf("a failed answer whose question waits is refused: %+v", all)
		}
		// Another device answers, and the session's event comes: the check
		// learns of it, and the words are refused with pocketd's words.
		elsewhere := q
		elsewhere.State, elsewhere.Answer, elsewhere.SettledBy = proto.QuestionAnswered, "Go", "phone"
		h.srv.PutQuestion(elsewhere)
		summary, _ := h.srv.Session(5)
		s := summary.Session
		h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
		h.loop.RunUntil(t, func() bool { return len(h.store.QuestionRefusals(5)) == 1 && h.store.InFlight() == 0 })
		r, _ := onlyRefusal(h.store, 5)
		if r.Answer != "Snow first." || !strings.Contains(r.Why, "answered already, on phone") {
			t.Errorf("the refusal %+v", r)
		}
		if got := sentIDs(t, h, path); len(got) != 2 || got[0] != got[1] {
			t.Errorf("request ids %q, want the check to send the same one", got)
		}
	})
}

// Each question's refused words are kept apart, and only the owner's
// dismissal takes them away: a later question's refusal, a refused skip's
// included, keeps the earlier words (Codex review of #95, round 2).
func TestRefusalsOfTwoQuestionsAreKeptApart(t *testing.T) {
	h := newHarness(t)
	first := question("q-a", 5, "Which goes first?", 0)
	h.srv.AddSession(waitingOn(5, first))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return ok })
	*h.store.QuestionDraft(first) = "Snow first."
	// It times out, and the next question comes.
	timedOut := first
	timedOut.State, timedOut.Reason, timedOut.SettledBy = proto.QuestionSkipped, proto.SkippedTimeout, "pocketd"
	h.srv.PutQuestion(timedOut)
	second := question("q-b", 5, "And the docs?", time.Minute)
	h.srv.PutQuestion(second)
	summary, _ := h.srv.Session(5)
	s := summary.Session
	h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
	h.loop.RunUntil(t, func() bool { got, ok := h.store.Data.Question(5); return ok && got.ID == "q-b" })
	// The owner skips the second on this device as another device
	// answers it: conflict.
	answered := second
	answered.State, answered.Answer, answered.SettledBy = proto.QuestionAnswered, "Later", "phone"
	h.srv.PutQuestion(answered)
	h.store.SkipQuestion(second)
	h.loop.RunUntil(t, func() bool { return len(h.store.QuestionRefusals(5)) == 2 && h.store.InFlight() == 0 })
	all := h.store.QuestionRefusals(5)
	if all[0].Question != "q-a" || all[0].Answer != "Snow first." || all[1].Question != "q-b" || !strings.Contains(all[1].Why, "answered already, on phone") {
		t.Fatalf("the refusals %+v", all)
	}
	h.store.DismissQuestionRefusal("q-b")
	if r, ok := onlyRefusal(h.store, 5); !ok || r.Question != "q-a" || r.Answer != "Snow first." {
		t.Errorf("dismissing the second took the first: %+v", h.store.QuestionRefusals(5))
	}
}

// questionRequests are every request a store sent to answer a question
// of session 5 or to message it.
func questionRequests(h *harness) []pocketdtest.Request {
	var out []pocketdtest.Request
	for _, r := range h.srv.Requests() {
		if r.Method == http.MethodPost && (strings.HasPrefix(r.Path, "/v1/sessions/5/question/") || r.Path == proto.Path(proto.PathMessages, 5)) {
			out = append(out, r)
		}
	}
	return out
}

// The words the owner writes for a question are that question's alone
// (Codex review of #95, round 1): when it settles while they are written,
// on another device, by its time running out, or as the session's next
// question comes, they go nowhere, neither as a message nor to the next
// question, and are kept, with why, until the owner dismisses them. A
// send of them that comes late is refused here, with nothing sent.
func TestAnAnswerDraftStaysWithItsQuestion(t *testing.T) {
	for _, tc := range []struct {
		name string
		// settle settles q-a on pocketd, and next is the question the
		// session's event brings in its place, if any.
		settle func(q proto.Question) proto.Question
		next   *proto.Question
	}{
		{"answered on another device", func(q proto.Question) proto.Question {
			q.State, q.Answer, q.SettledBy = proto.QuestionAnswered, "Go", "phone"
			return q
		}, nil},
		{"timed out", func(q proto.Question) proto.Question {
			q.State, q.Reason, q.SettledBy = proto.QuestionSkipped, proto.SkippedTimeout, "pocketd"
			return q
		}, nil},
		{"a new question arrives", func(q proto.Question) proto.Question {
			q.State, q.Reason, q.SettledBy = proto.QuestionSkipped, proto.SkippedTimeout, "pocketd"
			return q
		}, ptrTo(question("q-b", 5, "And the docs?", time.Minute))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			q := question("q-a", 5, "Which goes first?", 0, "Go", "Snow")
			h.srv.AddSession(waitingOn(5, q))
			h.connect(t)
			h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return ok })
			*h.store.QuestionDraft(q) = "Snow first, it blocks boots."
			// pocketd settles it, and its session's event comes.
			h.srv.PutQuestion(tc.settle(q))
			summary, _ := h.srv.Session(5)
			s := summary.Session
			if tc.next != nil {
				h.srv.PutQuestion(*tc.next)
				s.Question, s.State = tc.next, proto.SessionWaiting
			}
			h.srv.Emit(proto.Event{Type: proto.EventSession, Session: &s})
			h.loop.RunUntil(t, func() bool { cur, ok := h.store.Data.Question(5); return !ok || cur.ID != "q-a" })
			r, kept := onlyRefusal(h.store, 5)
			if !kept || r.Answer != "Snow first, it blocks boots." || r.Why != QuestionGone {
				t.Fatalf("the words written for the settled question: %+v, %v", r, kept)
			}
			if _, left := h.store.questionDrafts["q-a"]; left {
				t.Error("the settled question's draft stays")
			}
			c := h.store.Data.ComposerOf(5)
			switch {
			case tc.next == nil && c.Answers():
				t.Errorf("the composer answers %+v", c.Question)
			case tc.next != nil && (!c.Answers() || c.Question.ID != "q-b" || *h.store.QuestionDraft(*c.Question) != ""):
				t.Errorf("the next question's composer %+v", c)
			}
			// The words sent late, as by a key pressed as the event came:
			// refused here, and nothing goes.
			var done []error
			h.store.AnswerQuestion(q, "Snow first, it blocks boots.", nil, func(err error) { done = append(done, err) })
			h.settle(t)
			if len(done) != 1 || done[0] == nil {
				t.Errorf("a late answer to the settled question: %v", done)
			}
			if reqs := questionRequests(h); len(reqs) != 0 {
				t.Errorf("the words went: %+v", reqs)
			}
			if r, _ := onlyRefusal(h.store, 5); r.Answer != "Snow first, it blocks boots." {
				t.Errorf("the words kept twice or lost: %q", r.Answer)
			}
			h.store.DismissQuestionRefusal("q-a")
			if _, kept := onlyRefusal(h.store, 5); kept {
				t.Error("dismissed, the words stay")
			}
		})
	}
}

// The session's event of an answer's settlement may come before the
// answer's own reply: the words on their way are neither kept nor lost
// meanwhile, and once the reply says pocketd took them, nothing is kept
// (Codex review of #95, round 1).
func TestASettlementAheadOfTheAnswersReplyKeepsNothing(t *testing.T) {
	h := newHarness(t)
	q := question("q-a", 5, "Which goes first?", 0)
	h.srv.AddSession(waitingOn(5, q))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return ok })
	*h.store.QuestionDraft(q) = "Snow first."
	path := proto.QuestionPath(5, "q-a")
	release := h.srv.HoldAnswerOnce(http.MethodPost, path)
	h.store.AnswerQuestion(q, "Snow first.", nil, func(error) {})
	// pocketd took it, and its session's event is here ahead of the reply.
	h.loop.RunUntil(t, func() bool { _, ok := h.store.Data.Question(5); return !ok && h.srv.Holding(http.MethodPost, path) })
	if r, kept := onlyRefusal(h.store, 5); kept {
		t.Fatalf("the words on their way are kept as refused: %+v", r)
	}
	if _, left := h.store.questionDrafts["q-a"]; !left {
		t.Fatal("the words on their way left before the reply")
	}
	release()
	h.loop.RunUntil(t, func() bool { return !h.store.AnsweringQuestion("q-a").Busy && h.store.InFlight() == 0 })
	if r, kept := onlyRefusal(h.store, 5); kept {
		t.Errorf("the answer that went is kept as refused: %+v", r)
	}
	if _, left := h.store.questionDrafts["q-a"]; left {
		t.Error("the answer that went stays in its draft")
	}
	if got, _ := h.srv.Question("q-a"); got.Answer != "Snow first." {
		t.Errorf("pocketd's question %+v", got)
	}
}

// The agent's hint for a free answer shows beside the cue that the text
// answers the question, on the composer and the card; the cue alone shows
// without one (Codex review of #95, round 1).
func TestTheQuestionsHintShows(t *testing.T) {
	q := question("q-a", 2, "When can Vera pair with you?", 0)
	q.Placeholder = "  a day and a time, e.g. Tuesday 10:00.\nAnything else"
	c := Composer{Question: &q}
	if got := c.Hint(false); got != "Answer the question: a day and a time, e.g. Tuesday 10:00. Enter sends, Shift+Enter starts a new line." {
		t.Errorf("a desktop's hint %q", got)
	}
	if got := c.Hint(true); got != "Answer: a day and a time, e.g. Tuesday 10:00." {
		t.Errorf("a compact window's hint %q", got)
	}
	if got := QuestionCue(q, pocketdtest.Now); got != "Type your answer in the message box below: a day and a time, e.g. Tuesday 10:00. Skipped today 22:30 if no answer comes." {
		t.Errorf("the card's cue %q", got)
	}
	q.Placeholder = "a day and a time"
	if got := c.Hint(false); got != "Answer the question: a day and a time. Enter sends, Shift+Enter starts a new line." {
		t.Errorf("a desktop's hint %q", got)
	}
	q.Options = []string{"Today", "Tomorrow"}
	if got := QuestionCue(q, pocketdtest.Now); got != "Or type another answer in the message box below: a day and a time. Skipped today 22:30 if no answer comes." {
		t.Errorf("the card's cue with options %q", got)
	}
	q.Placeholder = "  "
	if c.Hint(false) != AnswerHint || c.Hint(true) != AnswerHintCompact {
		t.Errorf("with no hint: %q, %q", c.Hint(false), c.Hint(true))
	}
	if got := QuestionCue(q, pocketdtest.Now); got != "Or type another answer in the message box below. Skipped today 22:30 if no answer comes." {
		t.Errorf("the card's cue with no hint %q", got)
	}
	q.Placeholder = strings.Repeat("x", proto.MaxQuestionPlaceholder+10)
	if got := QuestionHint(q); utf8.RuneCountInString(got) != proto.MaxQuestionPlaceholder {
		t.Errorf("a long hint has %d characters", utf8.RuneCountInString(got))
	}
}

// An ask and a question have a noun each, everywhere the owner reads them
// (ADR-0047): the Inbox's kind of a thread's asks says ask, no kind says
// question, and the words the state writes for an ask say ask too.
func TestAsksAreCalledAsks(t *testing.T) {
	var d Data
	d.SetAsks(d.BeginAsks(), []proto.Ask{ask(1, 5, "Squash?", 0)})
	d.SetLessons(d.BeginLessons(), nil)
	d.SetApprovals(d.Begin(), nil)
	d.SetPlans(d.BeginPlans(), nil)
	d.SetPulls(d.BeginPulls(), nil)
	d.SetSessions(d.Begin(), []proto.Session{waitingOn(2, question("q-a", 2, "Ship it?", 0))})
	for _, k := range d.Inbox() {
		if strings.Contains(strings.ToLower(k.List+" "+k.One+" "+k.Many), "question") {
			t.Errorf("the Inbox's kind %+v says question", k)
		}
		if k.Count == 1 && k.List != "asks" && k.List != "sessions" {
			t.Errorf("the Inbox counts %+v", k)
		}
	}
	if note := InboxNote(d.Inbox()); note != "1 ask and 1 session wait for you" {
		t.Errorf("the Inbox says %q", note)
	}
	d.SetAsksError(d.BeginAsks(), "timeout")
	if note := InboxNote(d.Inbox()); note != "The asks are not loaded" {
		t.Errorf("a failed asks fetch: the Inbox says %q", note)
	}
	d.SetThreads(d.BeginThreads(), []proto.Thread{{ID: 1, State: proto.ThreadOpen, Agent: "vera"}})
	if note := d.BoardNote(); !strings.Contains(note, "the asks or pull requests are not loaded") {
		t.Errorf("the board says %q", note)
	}

	// An answer to an ask pocketd no longer has says so of the ask.
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Squash?", 0))
	h.srv.FailOnce(http.MethodGet, proto.PathAsks, http.StatusBadGateway)
	h.connect(t)
	h.settle(t)
	if !slices.ContainsFunc(h.logs, func(line string) bool { return strings.HasPrefix(line, "Loading the asks: ") }) {
		t.Errorf("the failed asks fetch's log %q", h.logs)
	}
	h.store.Refresh()
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	// A9 is no ask of pocketd's: 404.
	h.store.Answer(9, "yes")
	h.loop.RunUntil(t, func() bool { return len(h.store.AnswerRefusals()) == 1 && !h.store.Answering(9).Busy })
	if got := h.store.AnswerRefusals()[0].Why; got != "pocketd no longer has this ask." {
		t.Errorf("a gone ask's refusal says %q", got)
	}
	for _, line := range h.logs {
		if strings.Contains(strings.ToLower(line), "question") {
			t.Errorf("the store's log calls an ask a question: %q", line)
		}
	}
}
