package app

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/egoist/mygo/ui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// questionOf is a question Odrade's session 1 puts to the owner from its
// call q1 (ADR-0048).
func questionOf(id, text string, options ...string) proto.Question {
	return proto.Question{ID: id, SessionID: 1, CallID: "q1", Question: text, Options: options, State: proto.QuestionWaiting,
		CreatedAt: at(-time.Minute), Deadline: at(29 * time.Minute)}
}

// seedQuestion gives Odrade's session 1 (seedTeam) a transcript whose run
// waits on its question q, as pocketd lists it.
func (x *testApp) seedQuestion(q proto.Question) proto.Session {
	model := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	args, _ := json.Marshal(map[string]any{"question": q.Question, "options": q.Options})
	s := proto.Session{ID: 1, Title: "Plan the week", Cwd: "/home/team/agents/odrade", Model: model, Profile: "odrade", Route: "profile",
		Standing: true, State: proto.SessionWaiting, UpdatedAt: at(-time.Minute)}
	x.srv.AddSession(s,
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryUser, Text: "Plan the week's work for the team.", At: at(-3 * time.Minute)},
		proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryAssistant,
			Text:      "Vera has room for one of the two big items this week, and both are ready to start.",
			ToolCalls: []proto.ToolCall{{ID: "q1", Name: "question", Arguments: args}}, At: at(-time.Minute)},
	)
	x.srv.SetLive(proto.Live{SessionID: 1, Busy: true, Tools: []proto.LiveTool{{CallID: "q1", Name: "question", Status: proto.ToolRunning}}})
	x.srv.PutQuestion(q)
	got, _ := x.srv.Session(1)
	return got.Session
}

// openQuestionSession opens Odrade's session and waits for its
// transcript.
func (x *testApp) openQuestionSession(t *testing.T) {
	t.Helper()
	x.a.openSession(1)
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 1 })
}

// putQuestion puts q in its session on pocketd and sends the session's
// event, as pocketd does when the tool call starts to wait.
func (x *testApp) putQuestion(t *testing.T, q proto.Question) {
	t.Helper()
	x.srv.PutQuestion(q)
	s, _ := x.srv.Session(q.SessionID)
	summary := s.Session
	x.srv.Emit(proto.Event{Type: proto.EventSession, Session: &summary})
	x.until(t, func() bool { got, ok := x.a.st.Data.Question(q.SessionID); return ok && got.ID == q.ID })
}

// answerSent is the body of the one answer pocketd got for question qid.
func (x *testApp) answerSent(t *testing.T, qid string) proto.AnswerQuestion {
	t.Helper()
	reqs := x.srv.RequestsTo(http.MethodPost, proto.QuestionPath(1, qid))
	var body proto.AnswerQuestion
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.RequestID == "" {
		t.Fatalf("the answers sent to %s: %+v", qid, reqs)
	}
	return body
}

// A session's question shows inline in its transcript, after the agent's
// words and its call (ADR-0048): the question, its options and Skip, and
// no field of its own; the composer answers it, which its hint says. A
// click on an option answers at once, and the card goes; options the
// owner may pick several of are checkboxes with Send Choices; the
// composer's words answer the next one, and Skip skips it.
func TestAQuestionInItsSessionSnapshot(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	x.seedQuestion(questionOf("q-week", "Which goes first for Vera this week?", "Bump Go to 1.27", "Fix the snow boot", "Both, Go first"))
	x.connect(t)
	x.untilProfiles(t)
	x.openQuestionSession(t)
	x.hasAll(t, "Plan the week", "waiting", "Plan the week's work for the team.", "question", "waiting for you",
		"Odrade waits for your answer", "Which goes first for Vera this week?", "Bump Go to 1.27", "Fix the snow boot", "Both, Go first", "Skip", "Send Answer")
	if _, ok := x.tt.AccessByRole(ui.RoleGroup, "Question for you"); !ok {
		t.Errorf("no question card in %q", x.tt.Texts())
	}
	field, ok := x.tt.AccessByRole(ui.RoleTextField, "Answer")
	if !ok || field.Placeholder != state.AnswerHint {
		t.Errorf("the composer while the question waits: %+v, %v", field, ok)
	}
	if _, ok := x.tt.AccessByRole(ui.RoleTextField, "Message"); ok {
		t.Error("the composer still sends a message")
	}
	if x.tt.HasText("Working…") {
		t.Error("the working line shows while the question waits")
	}
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "Skipped today 22:29 if no answer comes.") {
		t.Errorf("the card does not say when the question is skipped: %q", x.tt.Texts())
	}
	x.snapshot(t, "session-question")

	// An option answers at once: the option alone goes, and the card goes.
	x.click(t, "Fix the snow boot")
	x.until(t, func() bool { _, waits := x.a.st.Data.Question(1); return !waits && x.a.st.InFlight() == 0 })
	if body := x.answerSent(t, "q-week"); body.Answer != "" || !slices.Equal(body.Option, []int{1}) || body.Skipped {
		t.Errorf("the option's answer %+v", body)
	}
	if _, ok := x.tt.AccessByRole(ui.RoleGroup, "Question for you"); ok {
		t.Error("the answered question's card stays")
	}
	if field, ok := x.tt.AccessByRole(ui.RoleTextField, "Message"); !ok || field.Placeholder == state.AnswerHint {
		t.Errorf("the composer after the answer: %+v, %v", field, ok)
	}

	// Several options: checkboxes and Send Choices.
	days := questionOf("q-days", "Which days can Vera pair with you?", "Monday", "Wednesday", "Friday")
	days.Multiple = true
	x.putQuestion(t, days)
	x.click(t, "Friday")
	x.click(t, "Monday")
	x.click(t, "Send Choices")
	x.until(t, func() bool { _, waits := x.a.st.Data.Question(1); return !waits && x.a.st.InFlight() == 0 })
	if body := x.answerSent(t, "q-days"); !slices.Equal(body.Option, []int{0, 2}) || body.Answer != "" {
		t.Errorf("the choices sent %+v", body)
	}

	// The composer's words answer the question, with no option; the draft
	// clears once pocketd took it.
	why := questionOf("q-why", "Why the snow fix first?")
	x.putQuestion(t, why)
	// The field edits the question's own draft, not the message draft
	// (Codex review of #95, round 1).
	*x.a.st.QuestionDraft(why) = "Its users cannot boot."
	x.settle()
	x.click(t, "Send Answer")
	x.until(t, func() bool { _, waits := x.a.st.Data.Question(1); return !waits && x.a.st.InFlight() == 0 })
	if body := x.answerSent(t, "q-why"); body.Answer != "Its users cannot boot." || len(body.Option) != 0 {
		t.Errorf("the composer's answer %+v", body)
	}
	if n := len(x.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathMessages, 1))); n != 0 {
		t.Errorf("the answer went as %d messages too", n)
	}
	if kept := len(x.a.st.QuestionRefusals(1)) > 0; kept || *x.a.composer.draft(1) != "" {
		t.Errorf("the answer that went is kept, or reached the message draft: %q", *x.a.composer.draft(1))
	}

	// Skip goes on without an answer.
	x.putQuestion(t, questionOf("q-skip", "Shall I tell Vera now?", "Yes", "No"))
	x.click(t, "Skip")
	x.until(t, func() bool { _, waits := x.a.st.Data.Question(1); return !waits && x.a.st.InFlight() == 0 })
	if body := x.answerSent(t, "q-skip"); !body.Skipped || body.Answer != "" || len(body.Option) != 0 {
		t.Errorf("the skip sent %+v", body)
	}
}

// An answer pocketd refused, as one another device gave first, says why
// above the composer, with the owner's words, and goes with Dismiss; the
// words never reach the message draft.
func TestAQuestionAnsweredOnAnotherDeviceSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.seedTeam()
	q := questionOf("q-week", "Which goes first?", "Go", "Snow")
	x.seedQuestion(q)
	x.connect(t)
	x.openQuestionSession(t)
	elsewhere := q
	elsewhere.State, elsewhere.Answer, elsewhere.SettledBy = proto.QuestionAnswered, "Go", "phone"
	x.srv.PutQuestion(elsewhere)
	*x.a.st.QuestionDraft(q) = "Snow, please."
	x.settle()
	x.click(t, "Send Answer")
	x.until(t, func() bool { return len(x.a.st.QuestionRefusals(1)) == 1 && x.a.st.InFlight() == 0 })
	text := strings.Join(x.tt.Texts(), " ")
	if !strings.Contains(text, "Your answer was not taken: the question was answered already, on phone: the first answer stands.") || !x.tt.HasText("Snow, please.") {
		t.Errorf("no refusal with the owner's words in %q", x.tt.Texts())
	}
	if got := *x.a.composer.draft(1); got != "" {
		t.Errorf("the refused answer reached the message draft: %q", got)
	}
	x.click(t, "Dismiss")
	if len(x.a.st.QuestionRefusals(1)) != 0 {
		t.Error("Dismiss kept the refusal")
	}
}

// The Inbox never lists a question among its asks: it has a line that
// says who waits for the owner's answer, and opens the session, where
// the question shows. The session's row says it waits, with the question
// badge.
func TestTheInboxOpensASessionWhoseQuestionWaits(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor", Home: "/home/team/agents/odrade"})
	x.seedQuestion(questionOf("q-week", "Which goes first for Vera this week?", "Go", "Snow"))
	x.connect(t)
	x.untilProfiles(t)
	x.hasAll(t, "Odrade waits for your answer", "Which goes first for Vera this week?", "Open Session", "1 session waits for you",
		"Plan the week, waits for you: question: Which goes first for Vera this week?")
	if _, ok := x.tt.AccessByRole(ui.RoleGroup, "Waiting for your answer: Plan the week"); !ok {
		t.Errorf("no line for the question in %q", x.tt.Texts())
	}
	for _, label := range []string{"Ask A1", "Question for you"} {
		if _, ok := x.tt.AccessByRole(ui.RoleGroup, label); ok {
			t.Errorf("the Inbox shows %q", label)
		}
	}
	if r, _ := x.a.sidebarData().Session(1); r.Badge != state.QuestionBadge || r.Mark != state.MarkWaiting {
		t.Errorf("the session's row %+v", r)
	}
	x.click(t, "Open Session")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 1 })
	if x.a.place != placeSession {
		t.Fatalf("place %v", x.a.place)
	}
	x.hasAll(t, "Which goes first for Vera this week?", "Skip")
}

// noQuestion fails when any text the window draws calls something a
// question: with no session's question waiting, only asks wait.
func (x *testApp) noQuestion(t *testing.T) {
	t.Helper()
	for _, text := range x.tt.Texts() {
		if strings.Contains(strings.ToLower(text), "question") {
			t.Errorf("the window says %q", text)
		}
	}
}

// An ask is an ask wherever the owner reads it, and never a question,
// which is a session's (ADR-0048): the Inbox's empty state, its list's
// failure, the ask's card and its label, and the status bar.
func TestAnAskIsCalledAnAsk(t *testing.T) {
	x := newTestApp(t)
	x.connect(t)
	x.hasAll(t, "Nothing waits for you")
	if !strings.Contains(strings.Join(x.tt.Texts(), " "), "asks from the team's threads") {
		t.Errorf("the empty Inbox says %q", x.tt.Texts())
	}
	x.noQuestion(t)

	y := newTestApp(t)
	y.srv.AddAsk(proto.Ask{ID: 1, Thread: 1, SessionID: 7, Question: "Squash the commits?", Options: []string{"Yes", "No"},
		State: proto.AskOpen, CreatedAt: at(-time.Minute)})
	y.srv.FailOnce(http.MethodGet, proto.PathAsks, http.StatusBadGateway)
	y.connect(t)
	if text := strings.Join(y.tt.Texts(), " "); !strings.Contains(text, "Loading the asks failed: ") ||
		!strings.Contains(text, "Asks may wait for you that this list does not show.") {
		t.Errorf("the failed asks say %q", y.tt.Texts())
	}
	y.noQuestion(t)
	y.click(t, "Try Again")
	y.until(t, func() bool { return len(y.a.st.Data.Asks()) == 1 && y.a.st.InFlight() == 0 })
	y.hasAll(t, "A1 asks you", "1 ask waits for you", "1 ask waits")
	if _, ok := y.tt.AccessByRole(ui.RoleGroup, "Ask A1"); !ok {
		t.Errorf("no group labelled Ask A1 in %q", y.tt.Texts())
	}
	y.noQuestion(t)
}

// An Inbox whose sessions failed to load says so, with Try Again in
// place: a session's question may wait there unseen, and it says so of a
// session, never of an ask (Codex review of #95, round 1).
func TestAnInboxWhoseSessionsFailedToLoadSaysSo(t *testing.T) {
	x := newTestApp(t)
	x.srv.AddProfile(proto.Profile{ID: "odrade", Name: "Odrade", Role: "supervisor", Home: "/home/team/agents/odrade"})
	x.seedQuestion(questionOf("q-week", "Which goes first for Vera this week?", "Go", "Snow"))
	x.srv.FailOnce(http.MethodGet, proto.PathSessions, http.StatusBadGateway)
	x.connect(t)
	const problem = "Loading the sessions failed: internal: made to fail. A session may wait for your answer to its question that this list does not show."
	if !x.tt.HasText(problem) {
		t.Fatalf("no sessions' problem in the Inbox: %q", x.tt.Texts())
	}
	if strings.Contains(strings.ToLower(problem), "ask") {
		t.Errorf("the sessions' problem calls a question an ask: %q", problem)
	}
	if x.tt.HasText("Nothing waits for you") {
		t.Error("the Inbox says nothing waits though its sessions did not load")
	}
	// Its own Try Again, in the Inbox, under the problem.
	field, ok := x.tt.Find(problem)
	if !ok {
		t.Fatal("the problem is not drawn")
	}
	var retry *ui.AccessInfo
	for _, n := range x.tt.AccessAll() {
		// The one in the problem's row, right of its words: the sidebar
		// has a Try Again of its own.
		if n.Role == ui.RoleButton && n.Name == "Try Again" && n.Bounds.X > field.X && n.Bounds.Y <= field.Y+field.H && n.Bounds.Y+n.Bounds.H >= field.Y {
			retry = &n
		}
	}
	if retry == nil {
		t.Fatalf("no Try Again beside the Inbox's sessions problem in %q", x.tt.Texts())
	}
	x.tt.ClickAt(retry.Bounds.X+retry.Bounds.W/2, retry.Bounds.Y+retry.Bounds.H/2)
	x.settle()
	x.until(t, func() bool {
		status, _ := x.a.st.Data.SessionsStatus()
		return status == state.ListLoaded && x.a.st.InFlight() == 0
	})
	x.hasAll(t, "Odrade waits for your answer", "1 session waits for you")
	if x.tt.HasText(problem) {
		t.Error("the problem stays once the sessions loaded")
	}
}

// On an iPhone (ADR-0044) the Sessions tab's row says the question waits,
// with its badge, and the session shows the card within the screen, with
// the composer answering it.
func TestThePhoneQuestionSnapshot(t *testing.T) {
	if !inPhoneProcess(t) {
		return
	}
	x := newTestApp(t)
	x.phoneApp(t, true, nil)
	q := questionOf("q-week", "Which goes first for Vera this week?", "Bump Go to 1.27", "Fix the snow boot", "Both, Go first")
	summary := x.seedQuestion(q)
	x.srv.Emit(proto.Event{Type: proto.EventSession, Session: &summary})
	x.until(t, func() bool { _, ok := x.a.st.Data.Question(1); return ok })
	x.tap(t, "Sessions")
	x.hasAll(t, "Plan the week", "question")
	x.snapshot(t, "phone-question-sessions")

	x.tap(t, "Plan the week")
	x.until(t, func() bool { o := x.a.st.Data.Open(); return o != nil && o.Loaded && o.ID == 1 })
	x.hasAll(t, "Odrade waits for your answer", "Which goes first for Vera this week?", "Skip", "Send Answer")
	x.within(t, ui.RoleGroup, "Question for you")
	x.within(t, ui.RoleButton, "Skip")
	field, ok := x.tt.AccessByRole(ui.RoleTextField, "Answer")
	if !ok || field.Placeholder != state.AnswerHintCompact || field.Bounds.Y+field.Bounds.H > phoneH-phoneBottom {
		t.Errorf("the composer answering on a phone: %+v, %v", field, ok)
	}
	x.snapshot(t, "phone-question-session")

	x.tap(t, "Bump Go to 1.27")
	x.until(t, func() bool { _, waits := x.a.st.Data.Question(1); return !waits && x.a.st.InFlight() == 0 })
	if body := x.answerSent(t, "q-week"); !slices.Equal(body.Option, []int{0}) {
		t.Errorf("the phone's answer %+v", body)
	}
}
