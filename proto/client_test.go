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
		if r.Header.Get(HeaderProtocol) != "2" || r.Header.Get("Authorization") != "Bearer tok" {
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
		{Seq: 3, Type: EventResync},
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
