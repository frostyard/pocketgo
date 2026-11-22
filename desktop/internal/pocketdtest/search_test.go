package pocketdtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

// The fake searches as pocketd does (ADR-0050): every word begins a word
// of a title or of a user or assistant entry, with its tool calls' names
// and without any tool's output; newest first, at most
// proto.MaxSearchSessionHits of one session's entries, and bad_request
// for a query with no word.
func TestTheFakeSearchesAsPocketdDoes(t *testing.T) {
	s := New()
	var long []proto.Entry
	for i := int64(1); i <= 7; i++ {
		long = append(long, proto.Entry{ID: i, SessionID: 1, Kind: proto.EntryUser, Text: "Deploy step", At: Now.Add(time.Duration(i) * time.Minute)})
	}
	s.AddSession(proto.Session{ID: 1, Title: "Long", UpdatedAt: Now}, long...)
	s.AddSession(proto.Session{ID: 2, Title: "Deployment notes", UpdatedAt: Now.Add(-time.Hour)},
		proto.Entry{ID: 20, SessionID: 2, Kind: proto.EntryAssistant, Text: "Calling.", At: Now.Add(-2 * time.Hour),
			ToolCalls: []proto.ToolCall{{ID: "c", Name: "team_delegate", Arguments: json.RawMessage(`{}`)}}},
		proto.Entry{ID: 21, SessionID: 2, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c", Text: "quasar deploy"}})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := client.Search(ctx, "DEPLOY", 0)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, h := range got.Hits {
		keys = append(keys, fmt.Sprintf("%d/%d", h.SessionID, h.EntryID))
	}
	if want := "[1/7 1/6 1/5 1/4 1/3 2/0]"; fmt.Sprint(keys) != want || !got.Cut {
		t.Errorf("deploy found %v cut %v, want %s cut", keys, got.Cut, want)
	}
	if h := got.Hits[0]; h.Snippet[h.Marks[0].Start:h.Marks[0].End] != "Deploy" {
		t.Errorf("the mark of %+v", h)
	}
	if got, _ := client.Search(ctx, "delegate", 0); len(got.Hits) != 1 || got.Hits[0].EntryID != 20 {
		t.Errorf("a tool's name: %+v", got.Hits)
	}
	if got, _ := client.Search(ctx, "quasar", 0); len(got.Hits) != 0 {
		t.Errorf("a tool's output is searched: %+v", got.Hits)
	}
	var e *proto.Error
	if _, err := client.Search(ctx, " -- ", 0); !errors.As(err, &e) || e.Status != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
		t.Errorf("a query with no word: %v", err)
	}
}

// The fake finds a match anywhere in an entry, not in its first
// characters alone, takes pocketd's whole-number limits alone, and breaks
// ties as pocketd does (Codex review of #111, round 1).
func TestTheFakeSearchesWholeEntriesWithPocketdsLimitsAndTies(t *testing.T) {
	s := New()
	long := strings.Repeat("lorem ipsum ", 20) + "the flux capacitor drifted"
	s.AddSession(proto.Session{ID: 1, Title: "One", UpdatedAt: Now},
		proto.Entry{ID: 1, SessionID: 1, Kind: proto.EntryAssistant, Text: long, At: Now},
		proto.Entry{ID: 2, SessionID: 1, Kind: proto.EntryUser, Text: "a capacitor", At: Now})
	client, err := proto.NewClient(s.URL, Token, &http.Client{Transport: s.Transport()})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	got, err := client.Search(ctx, "capacitor", 0)
	if err != nil || len(got.Hits) != 2 {
		t.Fatalf("capacitor: %+v, %v", got, err)
	}
	// At one time, the later entry first.
	if got.Hits[0].EntryID != 2 || got.Hits[1].EntryID != 1 {
		t.Errorf("hits %+v, want entry 2 then 1", got.Hits)
	}
	h := got.Hits[1]
	if len(h.Marks) != 1 || h.Snippet[h.Marks[0].Start:h.Marks[0].End] != "capacitor" || !strings.HasPrefix(h.Snippet, "…") {
		t.Errorf("the long entry's snippet %q marks %+v", h.Snippet, h.Marks)
	}
	if got, err := client.Search(ctx, "capacitor", 1); err != nil || len(got.Hits) != 1 || got.Hits[0].EntryID != 2 || !got.Cut {
		t.Errorf("limit 1: %+v, %v", got, err)
	}
	for _, raw := range []string{"05", "+5", "0", "201"} {
		request, _ := http.NewRequest(http.MethodGet, s.URL+proto.PathSearch+"?q=capacitor&limit="+url.QueryEscape(raw), nil)
		request.Header.Set(proto.HeaderProtocol, strconv.Itoa(proto.Version))
		request.Header.Set("Authorization", "Bearer "+Token)
		response, err := (&http.Client{Transport: s.Transport()}).Do(request)
		if err != nil {
			t.Fatal(err)
		}
		_ = response.Body.Close()
		if response.StatusCode != http.StatusBadRequest {
			t.Errorf("limit %q: %d, want 400", raw, response.StatusCode)
		}
	}
}
