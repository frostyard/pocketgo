package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestPathReplacesTheID(t *testing.T) {
	if got := Path(PathMessages, int64(42)); got != "/v1/sessions/42/messages" {
		t.Fatalf("Path = %q", got)
	}
	if got := Path(PathApproval, "a/b"); got != "/v1/approvals/a%2Fb" {
		t.Fatalf("Path escapes the id: %q", got)
	}
}

func TestNewRequestIDIsRandomHex(t *testing.T) {
	a, b := NewRequestID(), NewRequestID()
	if len(a) != 32 || a == b || len(a) > MaxRequestIDSize {
		t.Fatalf("request ids %q %q", a, b)
	}
}

func TestClientSendsProtocolAuthAndBody(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get(HeaderProtocol) != "9" || r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("headers: %v", r.Header)
		}
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions/7/messages" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		var body SendMessage
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Text != "hi" || body.RequestID != "r1" {
			t.Errorf("body %+v, %v", body, err)
		}
		_ = json.NewEncoder(w).Encode(SentMessage{SubmissionID: 9})
	}))
	sent, err := client.Send(context.Background(), 7, SendMessage{RequestID: "r1", Text: "hi"})
	if err != nil || sent.SubmissionID != 9 {
		t.Fatalf("Send = %+v, %v", sent, err)
	}
}

func TestClientDecodesErrors(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(Error{Code: ErrorCodeNotFound, Message: "no session 3"})
	}))
	_, err := client.Session(context.Background(), 3)
	var e *Error
	if !errors.As(err, &e) || e.Code != ErrorCodeNotFound || e.Status != http.StatusNotFound {
		t.Fatalf("error = %#v", err)
	}
}

func TestNewClientRejectsOtherSchemes(t *testing.T) {
	if _, err := NewClient("ftp://host", "t", nil); err == nil {
		t.Fatal("ftp accepted")
	}
}

func TestEventsRoundTrip(t *testing.T) {
	var stream bytes.Buffer
	stream.WriteString(": keepalive\n\n")
	sent := []Event{
		{Seq: 1, Type: EventSession, Session: &Session{ID: 1, Title: "a"}},
		{Seq: 2, Type: EventLive, Live: &Live{SessionID: 1, Busy: true, Partial: &Entry{Kind: EntryAssistant, Text: "line one\nline two"}}},
		{Seq: 3, Type: EventThread, Thread: &Thread{ID: 2, Title: "Fix CI", Agent: "vera", Supervisor: 1, State: ThreadOpen}},
		{Seq: 4, Type: EventAsk, Ask: &Ask{ID: 5, Thread: 2, SessionID: 3, Question: "Squash?", Options: []string{"yes", "no"}, State: AskOpen}},
		{Seq: 5, Type: EventResync},
	}
	for _, event := range sent {
		if err := WriteEvent(&stream, event); err != nil {
			t.Fatal(err)
		}
	}
	var got []Event
	err := ReadEvents(&stream, func(event Event) error { got = append(got, event); return nil })
	if !errors.Is(err, ErrStreamEnded) {
		t.Fatalf("ReadEvents = %v", err)
	}
	a, _ := json.Marshal(sent)
	b, _ := json.Marshal(got)
	if !bytes.Equal(a, b) {
		t.Fatalf("events\n sent %s\n got  %s", a, b)
	}
}

func TestEventsSendsLastEventID(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Last-Event-ID") != "41" {
			t.Errorf("Last-Event-ID = %q", r.Header.Get("Last-Event-ID"))
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_ = WriteEvent(w, Event{Seq: 42, Type: EventResync})
	}))
	var seqs []int64
	err := client.Events(context.Background(), 41, func(e Event) error { seqs = append(seqs, e.Seq); return nil })
	if !errors.Is(err, ErrStreamEnded) || len(seqs) != 1 || seqs[0] != 42 {
		t.Fatalf("Events = %v, %v", seqs, err)
	}
}

func TestReadEventsStopsOnHandlerError(t *testing.T) {
	stop := errors.New("stop")
	var stream strings.Builder
	_ = WriteEvent(&stream, Event{Seq: 1, Type: EventResync})
	_ = WriteEvent(&stream, Event{Seq: 2, Type: EventResync})
	calls := 0
	err := ReadEvents(strings.NewReader(stream.String()), func(Event) error { calls++; return stop })
	if !errors.Is(err, stop) || calls != 1 {
		t.Fatalf("ReadEvents = %v after %d calls", err, calls)
	}
}

func TestUsageAdd(t *testing.T) {
	got := Usage{Input: 1, Output: 2, Total: 3, Cost: 0.5}.Add(Usage{Input: 10, CacheRead: 4, Total: 14, Cost: 0.25})
	want := Usage{Input: 11, Output: 2, CacheRead: 4, Total: 17, Cost: 0.75}
	if got != want {
		t.Fatalf("Add = %+v", got)
	}
}

// handlerTransport serves requests with a handler in memory, so unit tests
// open no socket.
type handlerTransport struct{ handler http.Handler }

func (h handlerTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	recorder := httptest.NewRecorder()
	h.handler.ServeHTTP(recorder, r)
	return recorder.Result(), nil
}

func handlerClient(t *testing.T, handler http.Handler) *Client {
	t.Helper()
	client, err := NewClient("http://pocketd.test/", "tok", &http.Client{Transport: handlerTransport{handler}})
	if err != nil {
		t.Fatal(err)
	}
	return client
}

// A profile's tools tell every tool (absent) from none ([]) on the wire.
func TestProfileToolsTellAllFromNone(t *testing.T) {
	all, _ := json.Marshal(Profile{ID: "a"})
	none, _ := json.Marshal(Profile{ID: "n", Tools: []string{}})
	if strings.Contains(string(all), "tools") || !strings.Contains(string(none), `"tools":[]`) {
		t.Fatalf("every tool %s, none %s", all, none)
	}
	var back Profile
	if err := json.Unmarshal(none, &back); err != nil || back.Tools == nil {
		t.Fatalf("none read back as %#v, %v", back.Tools, err)
	}
}

func TestClientAnswersAsksAndClosesThreads(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/asks/5":
			var body AnswerAsk
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Answer != "yes" || body.RequestID != "r1" {
				t.Errorf("answer body %+v, %v", body, err)
			}
			_ = json.NewEncoder(w).Encode(Ask{ID: 5, State: AskAnswered, Answer: "yes"})
		case "POST /v1/threads/2/close":
			_ = json.NewEncoder(w).Encode(Thread{ID: 2, State: ThreadClosed})
		case "GET /v1/asks":
			_ = json.NewEncoder(w).Encode([]Ask{{ID: 5}})
		case "GET /v1/threads":
			_ = json.NewEncoder(w).Encode([]Thread{{ID: 2}})
		default:
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
	}))
	ctx := context.Background()
	if ask, err := client.AnswerAsk(ctx, 5, AnswerAsk{RequestID: "r1", Answer: "yes"}); err != nil || ask.State != AskAnswered {
		t.Fatalf("AnswerAsk = %+v, %v", ask, err)
	}
	if thread, err := client.CloseThread(ctx, 2, CloseThread{RequestID: "r2"}); err != nil || thread.State != ThreadClosed {
		t.Fatalf("CloseThread = %+v, %v", thread, err)
	}
	if asks, err := client.Asks(ctx); err != nil || len(asks) != 1 {
		t.Fatalf("Asks = %+v, %v", asks, err)
	}
	if threads, err := client.Threads(ctx); err != nil || len(threads) != 1 {
		t.Fatalf("Threads = %+v, %v", threads, err)
	}
}

func TestClientListsAndDecidesLessons(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "POST /v1/lessons/3":
			var body DecideLesson
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Decision != LessonApprove || body.Text != "Run the tests." || body.RequestID != "r1" {
				t.Errorf("decision body %+v, %v", body, err)
			}
			_ = json.NewEncoder(w).Encode(Lesson{ID: 3, State: LessonActive, Text: "Run the tests."})
		case "GET /v1/lessons":
			_ = json.NewEncoder(w).Encode([]Lesson{{ID: 3, State: LessonPending}})
		default:
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
	}))
	ctx := context.Background()
	if lesson, err := client.DecideLesson(ctx, 3, DecideLesson{RequestID: "r1", Decision: LessonApprove, Text: "Run the tests."}); err != nil || lesson.State != LessonActive {
		t.Fatalf("DecideLesson = %+v, %v", lesson, err)
	}
	if lessons, err := client.Lessons(ctx); err != nil || len(lessons) != 1 || !lessons[0].Waits() {
		t.Fatalf("Lessons = %+v, %v", lessons, err)
	}
}

// A lesson waits for the owner while it is new, or while a change to it
// waits; one in force with nothing waiting, or gone, does not.
func TestALessonWaitsForTheOwner(t *testing.T) {
	for _, c := range []struct {
		lesson Lesson
		waits  bool
	}{
		{Lesson{State: LessonPending}, true},
		{Lesson{State: LessonActive}, false},
		{Lesson{State: LessonActive, Change: &LessonChange{Forget: true}}, true},
		{Lesson{State: LessonDiscarded}, false},
		{Lesson{State: LessonForgotten, Change: &LessonChange{Text: "x"}}, false},
	} {
		if got := c.lesson.Waits(); got != c.waits {
			t.Errorf("%+v waits %v", c.lesson, got)
		}
	}
}

func TestClientListsPulls(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method+" "+r.URL.Path != "GET /v1/pulls" {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		_ = json.NewEncoder(w).Encode([]PullRequest{{ID: 1, Thread: 2, Repository: "frostyard/x", Number: 3, State: PullOpen}})
	}))
	if pulls, err := client.Pulls(context.Background()); err != nil || len(pulls) != 1 || pulls[0].Number != 3 {
		t.Fatalf("Pulls = %+v, %v", pulls, err)
	}
}
