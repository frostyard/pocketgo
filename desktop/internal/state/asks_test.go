package state

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/desktop/internal/pocketdtest"
	"github.com/frostyard/pocketgo/proto"
)

func ask(id, sessionID int64, question string, created time.Duration, options ...string) proto.Ask {
	return proto.Ask{ID: id, Thread: 1, SessionID: sessionID, Question: question, Options: options, State: proto.AskOpen,
		CreatedAt: pocketdtest.Now.Add(created)}
}

func askIDs(list []proto.Ask) []int64 {
	var out []int64
	for _, a := range list {
		out = append(out, a.ID)
	}
	return out
}

// An ask answered while a fetch was on its way stays gone when the fetch's
// stale copy arrives; one put during a fetch stays (the Data merge rule).
func TestAsksFollowTheirEventsOverAStaleList(t *testing.T) {
	var d Data
	d.SetAsks(d.BeginAsks(), []proto.Ask{ask(1, 5, "Squash?", 0)})
	f := d.BeginAsks()
	answered := ask(1, 5, "Squash?", 0)
	answered.State, answered.Answer = proto.AskAnswered, "yes"
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &answered})
	put := ask(2, 5, "Which branch?", time.Minute)
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &put})
	d.SetAsks(f, []proto.Ask{ask(1, 5, "Squash?", 0)})
	if got := askIDs(d.Asks()); !slices.Equal(got, []int64{2}) {
		t.Errorf("asks %v, want only the one put since: [2]", got)
	}
	if st, _ := d.AsksStatus(); st != ListLoaded {
		t.Errorf("status %v", st)
	}
	// An older fetch answering last changes nothing.
	older := d.BeginAsks()
	newest := d.BeginAsks()
	d.SetAsks(newest, nil)
	d.SetAsks(older, []proto.Ask{ask(3, 5, "Old?", 0)})
	if len(d.Asks()) != 0 {
		t.Errorf("an older fetch put back %v", askIDs(d.Asks()))
	}
	d.SetAsksError(d.BeginAsks(), "timeout")
	if st, err := d.AsksStatus(); st != ListFailed || err != "timeout" {
		t.Errorf("a failed fetch: %v %q", st, err)
	}
}

func TestAsksShowOldestFirst(t *testing.T) {
	var d Data
	for _, a := range []proto.Ask{ask(3, 5, "c", 2*time.Minute), ask(1, 5, "a", 0), ask(2, 6, "b", time.Minute)} {
		d.Apply(proto.Event{Type: proto.EventAsk, Ask: &a})
	}
	if got := askIDs(d.Asks()); !slices.Equal(got, []int64{1, 2, 3}) {
		t.Errorf("asks %v, want oldest first", got)
	}
}

// The Inbox's title claims a count only when every list loaded.
func TestInboxNote(t *testing.T) {
	kinds := func(asks, lessons, approvals int, asksStatus, lessonsStatus, approvalsStatus ListStatus) []InboxKind {
		return []InboxKind{
			{List: "questions", One: "question", Many: "questions", Count: asks, Status: asksStatus},
			{List: "lessons", One: "lesson", Many: "lessons", Count: lessons, Status: lessonsStatus},
			{List: "approvals", One: "tool call", Many: "tool calls", Count: approvals, Status: approvalsStatus},
		}
	}
	L, F, W := ListLoaded, ListFailed, ListLoading
	for _, tc := range []struct {
		kinds []InboxKind
		want  string
	}{
		{kinds(0, 0, 0, L, L, L), "Nothing waits for you"},
		{kinds(1, 0, 0, L, L, L), "1 question waits for you"},
		{kinds(0, 0, 2, L, L, L), "2 tool calls wait for you"},
		{kinds(0, 1, 0, L, L, L), "1 lesson waits for you"},
		{kinds(2, 0, 1, L, L, L), "2 questions and 1 tool call wait for you"},
		{kinds(1, 3, 1, L, L, L), "1 question, 3 lessons and 1 tool call wait for you"},
		{kinds(0, 0, 0, F, L, L), "The questions are not loaded"},
		{kinds(0, 0, 0, L, L, W), "The approvals are not loaded"},
		{kinds(3, 0, 3, F, L, F), "The questions and the approvals are not loaded"},
		{kinds(0, 1, 0, L, F, L), "The lessons are not loaded"},
	} {
		if got := InboxNote(tc.kinds); got != tc.want {
			t.Errorf("InboxNote(%+v) = %q, want %q", tc.kinds, got, tc.want)
		}
	}
}

func TestValidateAnswer(t *testing.T) {
	if err := ValidateAnswer("  yes "); err != nil {
		t.Errorf("a good answer: %v", err)
	}
	if err := ValidateAnswer("   "); err == nil {
		t.Error("an empty answer passed")
	}
	if err := ValidateAnswer(strings.Repeat("é", proto.MaxAnswer+1)); err == nil {
		t.Error("an answer over the limit passed")
	}
	if err := ValidateAnswer(strings.Repeat("é", proto.MaxAnswer)); err != nil {
		t.Errorf("an answer at the limit, in characters: %v", err)
	}
}

func TestAnsweringAnAskTakesItOffTheList(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Squash?", 0, "yes", "no"))
	h.srv.AddAsk(ask(2, 5, "Which branch?", time.Minute))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 2 })
	h.store.Answer(1, " yes ")
	if !h.store.Answering(1).Busy {
		t.Error("the answer is not busy while it is on its way")
	}
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	var body proto.AnswerAsk
	reqs := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathAsk, 1))
	if len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Answer != "yes" || body.RequestID == "" {
		t.Fatalf("the answer sent %+v", reqs)
	}
	// An empty answer stays here and says why.
	h.store.Answer(2, "  ")
	if got := h.store.Answering(2).Err; !strings.Contains(got, "empty") {
		t.Errorf("an empty answer says %q", got)
	}
	// Answered elsewhere, and the event not here yet: conflict, and the
	// asks load again.
	other := ask(2, 5, "Which branch?", time.Minute)
	other.State = proto.AskAnswered
	h.srv.AddAsk(other)
	// The asks' reload is held, so the conflict is seen before it lands.
	release := h.srv.HoldAnswerOnce(http.MethodGet, proto.PathAsks)
	h.store.Answer(2, "main")
	h.loop.RunUntil(t, func() bool { return len(h.store.AnswerRefusals()) == 1 && !h.store.Answering(2).Busy })
	// The service's reason, kept as a refusal with the answer the owner
	// wrote (ADR-0037).
	if got := h.store.AnswerRefusals()[0]; got.Ask != 2 || got.Answer != "main" || !strings.Contains(got.Why, "already answered") {
		t.Errorf("a conflict's refusal %+v", got)
	}
	release()
	h.settle(t)
	if _, ok := h.store.Data.Ask(2); ok {
		t.Error("an ask answered elsewhere is still listed after the reload")
	}
	if _, kept := h.store.answers[2]; kept {
		t.Error("the answer's state outlived its ask")
	}
	if len(h.store.AnswerRefusals()) != 1 {
		t.Error("the refusal went with its ask's card")
	}
}

// An ask a supervisor's message withdrew leaves the Inbox with its event,
// and a fetch begun before the event does not bring it back (ADR-0037).
func TestAWithdrawnAskLeavesTheInbox(t *testing.T) {
	var d Data
	d.SetAsks(d.BeginAsks(), []proto.Ask{ask(1, 5, "Which branch?", 0), ask(2, 5, "Squash?", time.Minute)})
	f := d.BeginAsks()
	withdrawn := ask(1, 5, "Which branch?", 0)
	withdrawn.State = proto.AskWithdrawn
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &withdrawn})
	if got := askIDs(d.Asks()); !slices.Equal(got, []int64{2}) {
		t.Fatalf("asks %v, want the withdrawn one gone: [2]", got)
	}
	if _, open := d.Ask(1); open {
		t.Fatal("a withdrawn ask still reads as open")
	}
	d.SetAsks(f, []proto.Ask{ask(1, 5, "Which branch?", 0), ask(2, 5, "Squash?", time.Minute)})
	if got := askIDs(d.Asks()); !slices.Equal(got, []int64{2}) {
		t.Fatalf("a stale fetch brought the withdrawn ask back: %v", got)
	}
}

// The owner's answer to an ask a supervisor's message withdrew first is
// refused with pocketd's reason, which stays, with the answer, once the
// ask's card is gone, until the owner dismisses it (ADR-0037).
func TestAnAnswerToAWithdrawnAskShowsWhy(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Which branch?", 0))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	// Withdrawn on pocketd, and its event not here yet.
	withdrawn := ask(1, 5, "Which branch?", 0)
	withdrawn.State = proto.AskWithdrawn
	h.srv.AddAsk(withdrawn)
	// The refusal keeps the answer as the owner wrote it, not as it was
	// sent (Codex review of #62, round 1).
	h.store.Answer(1, "  main\n")
	h.loop.RunUntil(t, func() bool { return len(h.store.AnswerRefusals()) == 1 && !h.store.Answering(1).Busy })
	got := h.store.AnswerRefusals()[0]
	if got.Ask != 1 || got.Answer != "  main\n" || !strings.HasPrefix(got.Why, "A1 was withdrawn: the supervisor's message reached T1's specialist") {
		t.Fatalf("the refusal %+v", got)
	}
	var body proto.AnswerAsk
	if reqs := h.srv.RequestsTo(http.MethodPost, proto.Path(proto.PathAsk, 1)); len(reqs) != 1 || json.Unmarshal(reqs[0].Body, &body) != nil || body.Answer != "main" {
		t.Fatalf("the answer sent %+v", reqs)
	}
	h.settle(t)
	if len(h.store.Data.Asks()) != 0 {
		t.Fatalf("the withdrawn ask is still listed after the reload: %v", askIDs(h.store.Data.Asks()))
	}
	if refusals := h.store.AnswerRefusals(); len(refusals) != 1 || refusals[0] != got {
		t.Fatalf("the refusal went with its ask's card: %+v", refusals)
	}
	if h.store.Answering(1).Err != "" {
		t.Fatalf("the refusal is also the card's error %q", h.store.Answering(1).Err)
	}
	h.store.DismissAnswerRefusal(1)
	if refusals := h.store.AnswerRefusals(); len(refusals) != 0 {
		t.Fatalf("dismissed, it stays: %+v", refusals)
	}
}

func TestAnAnswerTriedAgainKeepsItsRequestID(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Squash?", 0))
	h.connect(t)
	p := proto.Path(proto.PathAsk, 1)
	h.srv.FailOnce(http.MethodPost, p, http.StatusBadGateway)
	h.store.Answer(1, "yes")
	h.loop.RunUntil(t, func() bool { return h.store.Answering(1).Err != "" })
	h.store.Answer(1, "yes")
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 0 })
	if got := sentIDs(t, h, p); len(got) != 2 || got[0] != got[1] {
		t.Errorf("request ids %q, want the retry to keep the first id", got)
	}
}

// An answer whose reply was lost keeps nothing once its ask is answered:
// the ask's event drops the retry state no card can use any more.
func TestALostAnswerReplyLeavesNoRetryState(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Squash?", 0))
	h.connect(t)
	p := proto.Path(proto.PathAsk, 1)
	h.srv.LoseAnswerOnce(http.MethodPost, p)
	h.store.Answer(1, "yes")
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 0 && !h.store.Answering(1).Busy })
	h.settle(t)
	if _, kept := h.store.answers[1]; kept {
		t.Error("the answer's retry state outlived its ask")
	}
	if id := h.store.ids.For("answer:1", "yes"); id == sentIDs(t, h, p)[0] {
		t.Error("the answer's request id outlived its ask")
	}
}

// The marker of an answered ask lives only while a fetch begun before it
// could still land: once a later fetch answered, it goes.
func TestAskMarkersRetire(t *testing.T) {
	var d Data
	older := d.BeginAsks()
	answered := ask(1, 5, "Squash?", 0)
	answered.State = proto.AskAnswered
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &answered})
	if _, kept := d.askTouched[1]; !kept {
		t.Fatal("no marker while a fetch begun before the answer is in flight")
	}
	newer := d.BeginAsks()
	d.SetAsks(newer, nil)
	if _, kept := d.askTouched[1]; kept {
		t.Fatal("the marker outlived every fetch it guards against")
	}
	// The older fetch, answering last, is dropped all the same.
	d.SetAsks(older, []proto.Ask{ask(1, 5, "Squash?", 0)})
	if len(d.Asks()) != 0 {
		t.Fatalf("a stale copy came back: %v", askIDs(d.Asks()))
	}
}

// With no asks fetch in flight, as on a healthy stream after the first
// load, an answered ask leaves no marker at all.
func TestNoAskMarkerWithoutAFetchInFlight(t *testing.T) {
	var d Data
	d.SetAsks(d.BeginAsks(), []proto.Ask{ask(1, 5, "Squash?", 0)})
	answered := ask(1, 5, "Squash?", 0)
	answered.State = proto.AskAnswered
	d.Apply(proto.Event{Type: proto.EventAsk, Ask: &answered})
	if len(d.askTouched) != 0 || len(d.Asks()) != 0 {
		t.Fatalf("markers %v, asks %v", d.askTouched, askIDs(d.Asks()))
	}
}

// An answer whose reply was lost, to an ask a supervisor's message
// withdrew, is kept as a refusal once the withdrawal's event comes: the
// event proves pocketd did not take it. A failed answer to an ask answered
// elsewhere is no refusal (ADR-0037; Codex review of #62, round 1).
func TestAWithdrawalEventKeepsAnAnswerWhoseReplyWasLost(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Which branch?", 0))
	h.srv.AddAsk(ask(2, 5, "Squash?", time.Minute))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 2 })
	for _, id := range []int64{1, 2} {
		h.srv.FailOnce(http.MethodPost, proto.Path(proto.PathAsk, id), http.StatusBadGateway)
	}
	h.store.Answer(1, " main")
	h.store.Answer(2, "yes")
	h.loop.RunUntil(t, func() bool {
		return h.store.Answering(1).Err != "" && h.store.Answering(2).Err != "" && !h.store.Answering(1).Busy && !h.store.Answering(2).Busy
	})
	withdrawn := ask(1, 5, "Which branch?", 0)
	withdrawn.State = proto.AskWithdrawn
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &withdrawn})
	answered := ask(2, 5, "Squash?", time.Minute)
	answered.State, answered.Answer = proto.AskAnswered, "no"
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &answered})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 0 })
	refusals := h.store.AnswerRefusals()
	if len(refusals) != 1 || refusals[0].Ask != 1 || refusals[0].Answer != " main" || !strings.HasPrefix(refusals[0].Why, "A1 was withdrawn") {
		t.Fatalf("refusals %+v", refusals)
	}
	if _, kept := h.store.answers[1]; kept {
		t.Error("the answer's retry state outlived its ask")
	}
}

// A fetch that drops a withdrawn ask before its event comes keeps the
// owner's failed answer until the event says how the ask left: then it is
// a refusal, with the answer as written (Codex review of #62, round 2).
func TestAFailedAnswerWaitsForTheEventThatSaysHowItsAskLeft(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Which branch?", 0))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	h.srv.FailOnce(http.MethodPost, proto.Path(proto.PathAsk, 1), http.StatusBadGateway)
	h.store.Answer(1, "main")
	h.loop.RunUntil(t, func() bool { return h.store.Answering(1).Err != "" && !h.store.Answering(1).Busy })
	// pocketd withdrew it; the list comes before the event.
	withdrawn := ask(1, 5, "Which branch?", 0)
	withdrawn.State = proto.AskWithdrawn
	h.srv.AddAsk(withdrawn)
	h.store.Refresh()
	h.settle(t)
	if len(h.store.Data.Asks()) != 0 || len(h.store.AnswerRefusals()) != 0 {
		t.Fatalf("asks %v, refusals %+v before the event", askIDs(h.store.Data.Asks()), h.store.AnswerRefusals())
	}
	if _, kept := h.store.answerTexts[1]; !kept {
		t.Fatal("the list dropped the failed answer before its outcome was known")
	}
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &withdrawn})
	h.loop.RunUntil(t, func() bool { return len(h.store.AnswerRefusals()) == 1 })
	if got := h.store.AnswerRefusals()[0]; got.Ask != 1 || got.Answer != "main" || !strings.HasPrefix(got.Why, "A1 was withdrawn") {
		t.Fatalf("the refusal %+v", got)
	}
}

// A withdrawal of an ask the owner did not answer keeps nothing, and what
// the store kept of an answer goes once it settles (Codex review of #62,
// round 2).
func TestAWithdrawalKeepsNothingOnceSettled(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	h.srv.AddAsk(ask(1, 5, "Which branch?", 0))
	h.srv.AddAsk(ask(2, 5, "Squash?", time.Minute))
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 2 })
	first := ask(1, 5, "Which branch?", 0)
	first.State = proto.AskWithdrawn
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &first})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 1 })
	if len(h.store.answerOutcomes) != 0 || len(h.store.answerTexts) != 0 || len(h.store.AnswerRefusals()) != 0 {
		t.Fatalf("an unanswered withdrawal kept %v %v %+v", h.store.answerOutcomes, h.store.answerTexts, h.store.AnswerRefusals())
	}
	h.srv.FailOnce(http.MethodPost, proto.Path(proto.PathAsk, 2), http.StatusBadGateway)
	h.store.Answer(2, "yes")
	h.loop.RunUntil(t, func() bool { return h.store.Answering(2).Err != "" && !h.store.Answering(2).Busy })
	second := ask(2, 5, "Squash?", time.Minute)
	second.State = proto.AskWithdrawn
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &second})
	h.loop.RunUntil(t, func() bool { return len(h.store.AnswerRefusals()) == 1 })
	if len(h.store.answerOutcomes) != 0 || len(h.store.answerTexts) != 0 || len(h.store.answers) != 0 {
		t.Fatalf("a settled answer kept %v %v %v", h.store.answerOutcomes, h.store.answerTexts, h.store.answers)
	}
}

// withdrawnAsk is ask with the state its withdrawal's event gives it.
func withdrawnAsk(a proto.Ask) *proto.Ask {
	a.State = proto.AskWithdrawn
	return &a
}

// A question a supervisor's message withdraws while the owner has an
// answer written to it keeps a card with the draft as written, until the
// owner dismisses it; with no draft, or only spaces, its card goes as an
// answered one's does. A later event of the same ask keeps the card as it
// is, and brings back none dismissed (ADR-0037; the owner's choice,
// 2026-11-21).
func TestAWithdrawnQuestionKeepsItsDraftUntilDismissed(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	drafted, blank, none := ask(1, 5, "Which branch?", 0), ask(2, 5, "Squash?", time.Minute), ask(3, 5, "Tag?", 2*time.Minute)
	for _, a := range []proto.Ask{drafted, blank, none} {
		h.srv.AddAsk(a)
	}
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 3 })
	*h.store.AnswerDraft(1) = "  main, and rebase first\n"
	*h.store.AnswerDraft(2) = "  "
	for _, a := range []proto.Ask{drafted, blank, none} {
		h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: withdrawnAsk(a)})
	}
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 0 })
	got := h.store.WithdrawnDrafts()
	if len(got) != 1 || got[0].Ask.ID != 1 || got[0].Ask.Question != "Which branch?" || got[0].Draft != "  main, and rebase first\n" {
		t.Fatalf("withdrawn drafts %+v, want only A1's, as written", got)
	}
	if len(h.store.answerDrafts) != 0 {
		t.Fatalf("drafts kept beside the card: %v", h.store.answerDrafts)
	}
	// The same withdrawal again, as a replayed event, changes nothing.
	*h.store.AnswerDraft(1) = "typed again"
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: withdrawnAsk(drafted)})
	h.settle(t)
	if again := h.store.WithdrawnDrafts(); len(again) != 1 || again[0].Draft != "  main, and rebase first\n" {
		t.Fatalf("a later event changed the card: %+v", again)
	}
	h.store.DismissWithdrawnDraft(1)
	if len(h.store.WithdrawnDrafts()) != 0 {
		t.Fatal("dismissed, the card stays")
	}
	// Dismissed, a later event of the same ask brings back nothing.
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: withdrawnAsk(drafted)})
	h.settle(t)
	if len(h.store.WithdrawnDrafts()) != 0 {
		t.Fatalf("a later event brought back a dismissed card: %+v", h.store.WithdrawnDrafts())
	}
}

// A draft of a question answered elsewhere goes with it, and one whose
// answer is out becomes no withdrawn draft: its answer's refusal keeps it
// (ADR-0037).
func TestOnlyAnUnsentDraftOfAWithdrawnQuestionIsKept(t *testing.T) {
	h := newHarness(t)
	h.srv.AddSession(session(5, "T1 · Fix CI", 0))
	answered, sent := ask(1, 5, "Which branch?", 0), ask(2, 5, "Squash?", time.Minute)
	h.srv.AddAsk(answered)
	h.srv.AddAsk(sent)
	h.connect(t)
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 2 })
	*h.store.AnswerDraft(1) = "main"
	*h.store.AnswerDraft(2) = "yes"
	h.srv.FailOnce(http.MethodPost, proto.Path(proto.PathAsk, 2), http.StatusBadGateway)
	h.store.Answer(2, *h.store.AnswerDraft(2))
	h.loop.RunUntil(t, func() bool { return h.store.Answering(2).Err != "" && !h.store.Answering(2).Busy })
	elsewhere := answered
	elsewhere.State, elsewhere.Answer = proto.AskAnswered, "develop"
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: &elsewhere})
	h.srv.Emit(proto.Event{Type: proto.EventAsk, Ask: withdrawnAsk(sent)})
	h.loop.RunUntil(t, func() bool { return len(h.store.Data.Asks()) == 0 && len(h.store.AnswerRefusals()) == 1 })
	if len(h.store.WithdrawnDrafts()) != 0 || len(h.store.answerDrafts) != 0 {
		t.Fatalf("withdrawn drafts %+v, drafts %v", h.store.WithdrawnDrafts(), h.store.answerDrafts)
	}
	if r := h.store.AnswerRefusals()[0]; r.Ask != 2 || r.Answer != "yes" {
		t.Fatalf("the refusal %+v", r)
	}
}

// The note of a withdrawn question names its thread's supervisor by that
// session's agent, else its title, else says "the supervisor" (ADR-0037).
func TestAWithdrawalNoteNamesTheSupervisor(t *testing.T) {
	var d Data
	a := ask(3, 7, "Which number?", 0)
	a.Thread = 4
	if got := d.WithdrawalNote(a); !strings.HasPrefix(got, "The supervisor's message to T4 withdrew A3 before you sent your answer") {
		t.Errorf("with no thread known %q", got)
	}
	d.SetThreads(d.BeginThreads(), []proto.Thread{{ID: 4, Title: "Renumber", Agent: "duncan", Supervisor: 2, Session: 7, State: proto.ThreadOpen}})
	d.SetSessions(d.Begin(), []proto.Session{{ID: 2, Title: "Supervisor desk"}})
	if got := d.WithdrawalNote(a); !strings.HasPrefix(got, "Supervisor desk's message to T4 withdrew A3") {
		t.Errorf("with the supervisor's session alone %q", got)
	}
	d.SetSessions(d.Begin(), []proto.Session{{ID: 2, Title: "Supervisor desk", Profile: "odrade"}})
	d.SetProfiles(d.Begin(), []proto.Profile{{ID: "odrade", Name: "Odrade"}})
	if got := d.WithdrawalNote(a); !strings.HasPrefix(got, "Odrade's message to T4 withdrew A3") || !strings.Contains(got, "Your draft is kept here until you dismiss it.") {
		t.Errorf("with the supervisor's agent %q", got)
	}
}
