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

// The Inbox's title claims a count only of the lists that loaded.
func TestInboxNote(t *testing.T) {
	for _, tc := range []struct {
		asks, approvals int
		asksStatus      ListStatus
		approvalsStatus ListStatus
		want            string
	}{
		{0, 0, ListLoaded, ListLoaded, "Nothing waits for you"},
		{1, 0, ListLoaded, ListLoaded, "1 question waits for you"},
		{0, 2, ListLoaded, ListLoaded, "2 tool calls wait for you"},
		{2, 1, ListLoaded, ListLoaded, "2 questions and 1 tool call wait for you"},
		{0, 0, ListFailed, ListLoaded, "The questions are not loaded"},
		{0, 0, ListLoaded, ListLoading, "The approvals are not loaded"},
		{3, 3, ListFailed, ListFailed, "The questions and the approvals are not loaded"},
	} {
		if got := InboxNote(tc.asks, tc.approvals, tc.asksStatus, tc.approvalsStatus); got != tc.want {
			t.Errorf("InboxNote(%d, %d, %v, %v) = %q, want %q", tc.asks, tc.approvals, tc.asksStatus, tc.approvalsStatus, got, tc.want)
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
	h.loop.RunUntil(t, func() bool { return h.store.Answering(2).Err != "" && !h.store.Answering(2).Busy })
	if !strings.Contains(h.store.Answering(2).Err, "answered this already") {
		t.Errorf("a conflict says %q", h.store.Answering(2).Err)
	}
	release()
	h.settle(t)
	if _, ok := h.store.Data.Ask(2); ok {
		t.Error("an ask answered elsewhere is still listed after the reload")
	}
	if _, kept := h.store.answers[2]; kept {
		t.Error("the answer's state outlived its ask")
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
