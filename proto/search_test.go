package proto

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"
)

// A query's words are its runs of letters and digits, lower case, each
// once, in order: punctuation, an underscore and a dash split words, as
// SQLite's unicode61 tokenizer splits the text the service indexes.
func TestSearchWords(t *testing.T) {
	for _, tc := range []struct {
		q    string
		want []string
	}{
		{"arm64", []string{"arm64"}},
		{"  The ARM64 job ", []string{"the", "arm64", "job"}},
		{"team_delegate", []string{"team", "delegate"}},
		{"ADR-0050", []string{"adr", "0050"}},
		{"\"pocketd's\" logs", []string{"pocketd", "s", "logs"}},
		{"Ünïcode ÉTÉ", []string{"ünïcode", "été"}},
		{"cafe\u0301 a\u0903b c\u20DDd e\u200Df", []string{"cafe\u0301", "a", "b", "c", "d", "e", "f"}},
		{"deploy Deploy DEPLOY", []string{"deploy"}},
		{"a b c d e f g h", []string{"a", "b", "c", "d", "e", "f", "g", "h"}},
		{"x " + strings.Repeat("y", MaxSearchWord), []string{"x", strings.Repeat("y", MaxSearchWord)}},
	} {
		got, err := SearchWords(tc.q)
		if err != nil || !slices.Equal(got, tc.want) {
			t.Errorf("SearchWords(%q) = %q, %v; want %q", tc.q, got, err, tc.want)
		}
	}
	for _, tc := range []struct{ q, want string }{
		{"", ErrNoSearchWords.Error()},
		{"  -- ** ", ErrNoSearchWords.Error()},
		{"a b c d e f g h i", "at most 8 words"},
		{strings.Repeat("y", MaxSearchWord+1), "at most 64 characters"},
		{strings.Repeat("é", MaxSearchWord+1), "at most 64 characters"},
		{strings.Repeat("a ", MaxSearchQuery/2) + "b", "at most 200 bytes"},
		{"ok \xff", "UTF-8"},
	} {
		if got, err := SearchWords(tc.q); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("SearchWords(%q) = %q, %v; want an error saying %q", tc.q, got, err, tc.want)
		}
	}
	if _, err := SearchWords(""); !errors.Is(err, ErrNoSearchWords) {
		t.Errorf("an empty query: %v", err)
	}
}

func TestClientSearches(t *testing.T) {
	at := time.Date(2026, 11, 14, 9, 0, 0, 0, time.UTC)
	client := handlerClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != PathSearch {
			t.Errorf("request %s %s", r.Method, r.URL.Path)
		}
		q := r.URL.Query()
		if q.Get("q") != "arm64 & co" {
			t.Errorf("q %q", q.Get("q"))
		}
		limit := q.Get("limit")
		if limit != "" && limit != "20" {
			t.Errorf("limit %q", limit)
		}
		_ = json.NewEncoder(w).Encode(SearchResults{Words: []string{"arm64", "co"}, Cut: true,
			Hits: []SearchHit{{SessionID: 3, EntryID: 9, Field: SearchAssistant, At: at, Snippet: "the arm64 runner", Marks: []SearchMark{{4, 9}}}}})
	}))
	got, err := client.Search(context.Background(), "arm64 & co", 20)
	if err != nil || !got.Cut || len(got.Hits) != 1 || got.Hits[0].EntryID != 9 || got.Hits[0].Marks[0] != (SearchMark{4, 9}) || !got.Hits[0].At.Equal(at) {
		t.Fatalf("Search = %+v, %v", got, err)
	}
	if _, err := client.Search(context.Background(), "arm64 & co", 0); err != nil {
		t.Fatal(err)
	}
}
