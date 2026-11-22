package proto

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
		if r.Header.Get(HeaderProtocol) != "25" || r.Header.Get("Authorization") != "Bearer tok" {
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

// An upload sends the file's bytes as its body, of its length and type,
// with its name and request id in the query string (rule 10a), and reads
// the upload pocketd answers.
func TestClientUploadsAFile(t *testing.T) {
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if r.Method != http.MethodPost || r.URL.Path != "/v1/sessions/7/uploads" || r.URL.Query().Get("name") != "a b&c.png" ||
			r.URL.Query().Get("requestId") != "u1" || r.Header.Get("Content-Type") != "image/png" || r.ContentLength != 5 ||
			r.Header.Get(HeaderProtocol) != fmt.Sprint(Version) || r.Header.Get("Authorization") != "Bearer tok" || string(body) != "12345" {
			t.Errorf("upload %s %s %v %d %q", r.Method, r.URL, r.Header, r.ContentLength, body)
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Upload{ID: 3, SessionID: 7, Name: "a b_c.png", MimeType: "image/png", Size: 5, Path: "/u/7/x-y-a b_c.png"})
	}))
	// Bytes past the size given never go.
	got, err := client.Upload(context.Background(), 7, "u1", "a b&c.png", "image/png", opener("1234567"), 5)
	if err != nil || got.ID != 3 || got.Path != "/u/7/x-y-a b_c.png" {
		t.Fatalf("Upload = %+v, %v", got, err)
	}
	if _, err := client.Upload(context.Background(), 7, "u2", "big", "", opener(""), MaxUpload+1); err == nil {
		t.Fatal("a file over MaxUpload went")
	}
	// An empty file goes as an empty body of length 0, with the default type.
	empty := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ContentLength != 0 || r.Header.Get("Content-Type") != "application/octet-stream" {
			t.Errorf("empty upload %d %v", r.ContentLength, r.Header)
		}
		_ = json.NewEncoder(w).Encode(Upload{ID: 4})
	}))
	if got, err := empty.Upload(context.Background(), 7, "u3", "empty", "", nil, 0); err != nil || got.ID != 4 {
		t.Fatalf("empty Upload = %+v, %v", got, err)
	}
}

// opener opens text, again each time.
func opener(text string) func() (io.ReadCloser, error) {
	return func() (io.ReadCloser, error) { return io.NopCloser(strings.NewReader(text)), nil }
}

// An upload follows a redirect to its own origin with its bytes again, as
// every other POST does (Codex review of #49, round 4).
func TestAnUploadFollowsARedirectWithItsBytes(t *testing.T) {
	var bodies []string
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, string(body))
		if r.URL.Path == "/v1/sessions/7/uploads" {
			http.Redirect(w, r, "/moved/v1/sessions/7/uploads?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Upload{ID: 3})
	}))
	got, err := client.Upload(context.Background(), 7, "u1", "a.txt", "text/plain", opener("hello"), 5)
	if err != nil || got.ID != 3 || len(bodies) != 2 || bodies[0] != "hello" || bodies[1] != "hello" {
		t.Fatalf("Upload = %+v, %v; bodies %q", got, err, bodies)
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

func TestClientListsAndApprovesPlans(t *testing.T) {
	rev := strings.Repeat("a", 64)
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.Method + " " + r.URL.Path {
		case "GET /v1/plans":
			_ = json.NewEncoder(w).Encode([]Plan{{ID: 1, Thread: 5, Lead: "duncan", State: PlanReview, Rev: rev,
				Graph: &PlanGraph{Version: 2, Rev: rev, Tasks: []PlanTask{{ID: "t1", Agent: "vera", State: TaskWaiting}}}}})
		case "POST /v1/plans/1/approve":
			var body ApprovePlan
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Rev != rev || body.Graph != 2 || body.RequestID != "r1" {
				t.Errorf("body %+v %v", body, err)
			}
			_ = json.NewEncoder(w).Encode(Plan{ID: 1, State: PlanApproved})
		default:
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
	}))
	plans, err := client.Plans(context.Background())
	if err != nil || len(plans) != 1 || plans[0].Graph.Tasks[0].ID != "t1" {
		t.Fatalf("Plans = %+v, %v", plans, err)
	}
	if got, err := client.ApprovePlan(context.Background(), 1, ApprovePlan{RequestID: "r1", Rev: rev, Graph: 2}); err != nil || got.State != PlanApproved {
		t.Fatalf("ApprovePlan = %+v, %v", got, err)
	}
}

// An upload follows no redirect to another origin, whatever client it
// runs on: the file would go to another host (Codex review of #49, round
// 5).
func TestAnUploadFollowsNoRedirectToAnotherOrigin(t *testing.T) {
	var hosts []string
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.Host)
		if r.Host == "pocketd.test" {
			http.Redirect(w, r, "http://elsewhere.test/v1/sessions/7/uploads?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Upload{ID: 3})
	}))
	if _, err := client.Upload(context.Background(), 7, "u1", "a.txt", "text/plain", opener("secret"), 6); err == nil {
		t.Fatal("the upload followed a redirect to another origin")
	}
	if len(hosts) != 1 || hosts[0] != "pocketd.test" {
		t.Fatalf("the upload went to %q", hosts)
	}
}

// An upload follows a redirect to its own origin however it is spelt: the
// host in another case, the scheme's default port named (Codex review of
// #49, rounds 6 and 7).
func TestAnUploadFollowsItsOriginHoweverSpelt(t *testing.T) {
	var bodies []string
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		bodies = append(bodies, r.URL.Host+" "+r.Header.Get("Authorization")+" "+string(body))
		if r.URL.Host == "pocketd.test" {
			http.Redirect(w, r, "http://POCKETD.test:80/v1/sessions/7/uploads?"+r.URL.RawQuery, http.StatusPermanentRedirect)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Upload{ID: 3})
	}))
	up, err := client.Upload(context.Background(), 7, "u1", "a.txt", "text/plain", opener("secret"), 6)
	if err != nil || up.ID != 3 {
		t.Fatalf("Upload = %+v, %v", up, err)
	}
	// With its token, which Go drops on a host spelt otherwise (Codex
	// review of #49, round 7).
	if len(bodies) != 2 || bodies[1] != "POCKETD.test:80 Bearer tok secret" {
		t.Fatalf("requests %q", bodies)
	}
}

// Hosts are one origin as the resolver looks them up: an ASCII name in
// any ASCII case, a final dot kept, an IP address with its zone exactly,
// and any other name byte for byte, since its IDNA form may name another
// host (Codex review of #49, rounds 7 and 8).
func TestSameOriginComparesHostsAsTheTransportLooksThemUp(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		same     bool
	}{
		{"https://example.org/x", "https://EXAMPLE.org/y", true},
		{"https://example.org/x", "https://example.org./y", false}, // a final dot skips the search list
		{"https://k.example/x", "https://\u212a.example/x", false}, // the Kelvin sign folds to k
		{"https://b\u00fccher.example/x", "https://B\u00dcCHER.example/x", false},
		{"https://b\u00fccher.example/x", "https://b\u00fccher.example/y", true},
		{"http://[fe80::1%25eth0]:8788/x", "http://[fe80::1%25ETH0]:8788/x", false},
		{"http://[fe80::1%25eth0]:8788/x", "http://[fe80::1%25eth0]:8788/y", true},
		{"http://[::1]:8788/x", "http://[0:0:0:0:0:0:0:1]:8788/x", true},
		{"http://127.0.0.1:8788/x", "http://localhost:8788/x", false},
	} {
		from, err := url.Parse(tc.from)
		if err != nil {
			t.Fatal(err)
		}
		to, err := url.Parse(tc.to)
		if err != nil {
			t.Fatal(err)
		}
		if got := SameOrigin(from, to); got != tc.same {
			t.Errorf("SameOrigin(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.same)
		}
	}
}

// An upload follows no redirect to its host with a final dot added: the
// rooted name skips the resolver's search list and may be another
// machine, which gets neither the file nor the token (Codex review of #49,
// round 8).
func TestAnUploadFollowsNoRedirectToItsRootedName(t *testing.T) {
	var seen []string
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen = append(seen, r.URL.Host+" "+r.Header.Get("Authorization")+" "+string(body))
		if r.URL.Host == "pocketd.test" {
			http.Redirect(w, r, "http://pocketd.test./v1/sessions/7/uploads?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Upload{ID: 3})
	}))
	if _, err := client.Upload(context.Background(), 7, "u1", "a.txt", "text/plain", opener("secret"), 6); err == nil {
		t.Fatal("the upload followed a redirect to its rooted name")
	}
	if len(seen) != 1 {
		t.Fatalf("requests %q", seen)
	}
}

// A redirect the caller's own check sends elsewhere is checked again: the
// upload follows it only to its origin, with its method, whatever that
// check changed (Codex review of #49, round 9).
func TestAnUploadChecksTheRedirectItsCallerChanged(t *testing.T) {
	var hosts []string
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hosts = append(hosts, r.URL.Host)
		if r.URL.Host == "pocketd.test" && !strings.HasPrefix(r.URL.Path, "/moved") {
			http.Redirect(w, r, "/moved"+r.URL.Path+"?"+r.URL.RawQuery, http.StatusTemporaryRedirect)
			return
		}
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(Upload{ID: 3})
	})
	client, err := NewClient("http://pocketd.test/", "tok", &http.Client{
		Transport: handlerTransport{handler},
		CheckRedirect: func(next *http.Request, _ []*http.Request) error {
			next.URL.Host = "elsewhere.test" // the caller's check moves it
			return nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := client.Upload(context.Background(), 7, "u1", "a.txt", "text/plain", opener("secret"), 6); err == nil {
		t.Fatal("the upload followed a redirect its caller moved to another origin")
	}
	if len(hosts) != 1 {
		t.Fatalf("requests to %q", hosts)
	}
}

// countedOpener opens body, counting the bodies opened and closed.
type countedOpener struct {
	body           string
	opened, closed int
}

func (c *countedOpener) open() (io.ReadCloser, error) {
	c.opened++
	return &countedBody{Reader: strings.NewReader(c.body), c: c}, nil
}

type countedBody struct {
	io.Reader
	c    *countedOpener
	done bool
}

func (b *countedBody) Close() error {
	if !b.done {
		b.done = true
		b.c.closed++
	}
	return nil
}

// A redirect an upload does not follow closes the file Go opened again
// for it, whether the upload refuses it or its caller keeps the redirect's
// answer (Codex review of #49, round 10).
func TestARedirectNotFollowedClosesItsFile(t *testing.T) {
	for _, c := range []struct {
		name  string
		to    string
		check func(*http.Request, []*http.Request) error
	}{
		{"refused", "http://elsewhere.test/v1/sessions/7/uploads", nil},
		{"kept", "/moved", func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
	} {
		handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = io.Copy(io.Discard, r.Body)
			http.Redirect(w, r, c.to, http.StatusTemporaryRedirect)
		})
		client, err := NewClient("http://pocketd.test/", "tok", &http.Client{Transport: handlerTransport{handler}, CheckRedirect: c.check})
		if err != nil {
			t.Fatal(err)
		}
		files := &countedOpener{body: "secret"}
		_, _ = client.Upload(context.Background(), 7, "u1", "a.txt", "text/plain", files.open, 6)
		if files.opened != 2 || files.closed != files.opened {
			t.Errorf("%s: %d opened, %d closed", c.name, files.opened, files.closed)
		}
	}
}
