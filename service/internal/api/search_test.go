package api

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

// startIndex starts the search index over the harness's sessions, and
// gives what waits until cond holds of it, after each of its reads.
func (h *harness) startIndex(t *testing.T) (wait func(what string, cond func() bool)) {
	t.Helper()
	stepped := make(chan struct{}, 1)
	h.server.indexStepped = func() {
		select {
		case stepped <- struct{}{}:
		default:
		}
	}
	h.server.StartIndex()
	return func(what string, cond func() bool) {
		t.Helper()
		deadline := time.After(10 * time.Second)
		for !cond() {
			select {
			case <-stepped:
			case <-deadline:
				t.Fatalf("waited for %s", what)
			}
		}
	}
}

func (h *harness) setTranscript(id int64, entries ...proto.Entry) {
	h.engine.mu.Lock()
	defer h.engine.mu.Unlock()
	if h.engine.transcripts == nil {
		h.engine.transcripts = map[int64][]proto.Entry{}
	}
	for i := range entries {
		entries[i].SessionID = id
	}
	h.engine.transcripts[id] = append(h.engine.transcripts[id], entries...)
}

func hitKeys(hits []proto.SearchHit) []string {
	var out []string
	for _, hit := range hits {
		out = append(out, fmt.Sprintf("%d/%d", hit.SessionID, hit.EntryID))
	}
	return out
}

// A word said once in an old session's reply finds that session, at the
// reply, with its snippet marked; titles are found too, newest first; a
// tool's output is not searched, its name is (rule 63).
func TestSearchFindsAWordInAnOldReply(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	old, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "a", Title: "Snow image fails to boot", Cwd: h.work, Model: model})
	recent, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "b", Title: "Fix the flaky CI", Cwd: h.work, Model: model})
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	h.setTranscript(old.ID,
		proto.Entry{ID: 1, Kind: proto.EntryUser, Text: "Why does the snow image not boot?", At: at},
		proto.Entry{ID: 2, Kind: proto.EntryAssistant, Text: "The flux capacitor in the initramfs drifted out of tune.",
			ToolCalls: []proto.ToolCall{{ID: "c1", Name: "bootc_status", Arguments: json.RawMessage(`{}`)}}, At: at.Add(time.Minute)},
		proto.Entry{ID: 3, Kind: proto.EntryToolResult, ToolResult: &proto.ToolResult{CallID: "c1", Name: "bootc_status", Text: "staged: quasar"}, At: at.Add(time.Minute)})
	h.setTranscript(recent.ID,
		proto.Entry{ID: 4, Kind: proto.EntryUser, Text: "The arm64 job is flaky again.", At: at.Add(24 * time.Hour)})

	// Before the index starts, a search says it is still indexing.
	got, err := h.client.Search(ctx, "capacitor", 0)
	if err != nil || len(got.Hits) != 0 || !got.Indexing {
		t.Fatalf("before the index: %+v, %v", got, err)
	}
	wait := h.startIndex(t)
	wait("the start's pass", func() bool { return !h.server.index.Behind() })

	got, err = h.client.Search(ctx, "Capacitor", 0)
	if err != nil || got.Indexing || len(got.Hits) != 1 || got.Cut {
		t.Fatalf("capacitor: %+v, %v", got, err)
	}
	hit := got.Hits[0]
	if hit.SessionID != old.ID || hit.EntryID != 2 || hit.Field != proto.SearchAssistant || !hit.At.Equal(at.Add(time.Minute)) {
		t.Errorf("hit %+v", hit)
	}
	if len(hit.Marks) != 1 || hit.Snippet[hit.Marks[0].Start:hit.Marks[0].End] != "capacitor" {
		t.Errorf("snippet %q marks %+v", hit.Snippet, hit.Marks)
	}
	if got, _ := h.client.Search(ctx, "quasar", 0); len(got.Hits) != 0 {
		t.Errorf("a tool's output is searched: %+v", got.Hits)
	}
	if got, _ := h.client.Search(ctx, "bootc_status", 0); len(got.Hits) != 1 || got.Hits[0].EntryID != 2 {
		t.Errorf("a tool's name is not found: %+v", got.Hits)
	}
	// A title's hit and an entry's, newest first: the title's time is the
	// session's last update.
	got, err = h.client.Search(ctx, "flaky", 0)
	if err != nil || len(got.Hits) != 2 {
		t.Fatalf("flaky: %+v, %v", got, err)
	}
	if keys := hitKeys(got.Hits); keys[0] != fmt.Sprintf("%d/0", recent.ID) || keys[1] != fmt.Sprintf("%d/4", recent.ID) || got.Hits[0].Field != proto.SearchTitle {
		t.Errorf("flaky found %v %+v", keys, got.Hits)
	}
	if got.Words[0] != "flaky" {
		t.Errorf("words %q", got.Words)
	}

	// An entry written later is found once its event notes the session.
	h.setTranscript(old.ID, proto.Entry{ID: 9, Kind: proto.EntryUser, Text: "Retune the capacitor.", At: at.Add(48 * time.Hour)})
	h.server.PublishEntry(proto.Entry{ID: 9, SessionID: old.ID, Kind: proto.EntryUser, Text: "Retune the capacitor."})
	wait("the new entry", func() bool { c, _ := h.store.SearchCursors(ctx); return c[old.ID] == 9 })
	got, _ = h.client.Search(ctx, "capacitor", 0)
	if keys := hitKeys(got.Hits); len(keys) != 2 || keys[0] != fmt.Sprintf("%d/9", old.ID) {
		t.Errorf("after the new entry: %v", keys)
	}
}

// An answer holds at most the limit and MaxSearchSessionHits of one
// session's entries, and says it left hits out.
func TestSearchKeepsItsLimits(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "a", Title: "Long", Cwd: h.work, Model: model})
	other, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "b", Title: "Short", Cwd: h.work, Model: model})
	at := time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)
	for id := int64(1); id <= 8; id++ {
		h.setTranscript(s.ID, proto.Entry{ID: id, Kind: proto.EntryUser, Text: "deploy it", At: at.Add(time.Duration(id) * time.Minute)})
	}
	h.setTranscript(other.ID, proto.Entry{ID: 20, Kind: proto.EntryUser, Text: "deployed", At: at.Add(time.Hour)})
	wait := h.startIndex(t)
	wait("the start's pass", func() bool { return !h.server.index.Behind() })
	got, err := h.client.Search(ctx, "deploy", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := fmt.Sprint([]string{fmt.Sprintf("%d/20", other.ID), fmt.Sprintf("%d/8", s.ID), fmt.Sprintf("%d/7", s.ID), fmt.Sprintf("%d/6", s.ID),
		fmt.Sprintf("%d/5", s.ID), fmt.Sprintf("%d/4", s.ID)})
	if keys := fmt.Sprint(hitKeys(got.Hits)); keys != want || !got.Cut {
		t.Errorf("found %s cut %v, want %s cut", keys, got.Cut, want)
	}
	got, err = h.client.Search(ctx, "deploy", 2)
	if err != nil || len(got.Hits) != 2 || !got.Cut {
		t.Errorf("limit 2: %+v, %v", got, err)
	}
	got, err = h.client.Search(ctx, "deployed", 2)
	if err != nil || len(got.Hits) != 1 || got.Cut {
		t.Errorf("one hit: %+v, %v", got, err)
	}
}

// A query with no word, too many or too long, and a limit outside 1 to
// 200 are bad_request; a search changes nothing, and failing to read the
// index is internal.
func TestSearchRefusesABadQuery(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	for _, tc := range []struct {
		q     string
		limit string
	}{
		{"", ""},
		{"  ?!  ", ""},
		{"a b c d e f g h i", ""},
		{"word", "0"},
		{"word", "201"},
		{"word", "ten"},
	} {
		path := proto.PathSearch + "?q=" + tc.q
		if tc.limit != "" {
			path += "&limit=" + tc.limit
		}
		response, e := h.raw("GET", path, "", map[string]string{proto.HeaderProtocol: fmt.Sprint(proto.Version), "Authorization": "Bearer " + h.token})
		if response.StatusCode != http.StatusBadRequest || e.Code != proto.ErrorCodeBadRequest {
			t.Errorf("q %q limit %q: %d %+v", tc.q, tc.limit, response.StatusCode, e)
		}
	}
	response, e := h.raw("POST", proto.PathSearch+"?q=x", "{}", map[string]string{proto.HeaderProtocol: fmt.Sprint(proto.Version), "Authorization": "Bearer " + h.token})
	if response.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d %+v", response.StatusCode, e)
	}
	// An index the indexer cannot read keeps the session behind, and says
	// so.
	h.engine.mu.Lock()
	h.engine.entriesErr = errors.New("PiG is busy")
	h.engine.mu.Unlock()
	if _, err := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "a", Title: "One", Cwd: h.work, Model: model}); err != nil {
		t.Fatal(err)
	}
	h.server.StartIndex()
	got, err := h.client.Search(ctx, "one", 0)
	if err != nil || !got.Indexing || len(got.Hits) != 1 || got.Hits[0].Field != proto.SearchTitle {
		t.Errorf("while a session fails to index: %+v, %v", got, err)
	}
}

// A search says the index is behind when it was behind as the search read
// it, though the start's pass ends before the answer: the hit it kept
// meanwhile is not in the answer (Codex review of #111, round 1).
func TestSearchSaysIndexingAsItReadTheIndex(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	s, _ := h.client.CreateSession(ctx, proto.CreateSession{RequestID: "a", Title: "Old", Cwd: h.work, Model: model})
	h.setTranscript(s.ID, proto.Entry{ID: 1, Kind: proto.EntryAssistant, Text: "The capacitor drifted.", At: time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC)})
	reached, release := make(chan struct{}), make(chan struct{})
	h.server.afterSearchRead = func() {
		close(reached)
		<-release
	}
	type answer struct {
		got proto.SearchResults
		err error
	}
	done := make(chan answer, 1)
	go func() {
		got, err := h.client.Search(ctx, "capacitor", 0)
		done <- answer{got, err}
	}()
	select {
	case <-reached:
	case <-time.After(10 * time.Second):
		t.Fatal("the search never read the index")
	}
	h.server.afterSearchRead = nil
	wait := h.startIndex(t)
	wait("the start's pass", func() bool { return !h.server.index.Behind() })
	close(release)
	a := <-done
	got, err := a.got, a.err
	if err != nil || len(got.Hits) != 0 || !got.Indexing {
		t.Fatalf("a search read before the pass ended: %+v, %v; want no hit, and indexing", got, err)
	}
	if got, err := h.client.Search(ctx, "capacitor", 0); err != nil || len(got.Hits) != 1 || got.Indexing {
		t.Fatalf("after the pass: %+v, %v", got, err)
	}
}
