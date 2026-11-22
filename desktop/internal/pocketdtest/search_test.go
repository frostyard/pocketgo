package pocketdtest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
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
