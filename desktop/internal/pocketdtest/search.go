package pocketdtest

import (
	"net/http"
	"sort"
	"strconv"
	"strings"
	"unicode"

	"github.com/frostyard/pocketgo/proto"
)

// SetSearchIndexing makes GET /v1/search say the service still reads
// older sessions into its index (ADR-0050).
func (s *Server) SetSearchIndexing(on bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.searchIndexing = on
}

// search answers GET /v1/search as pocketd does, over the fake's own
// sessions: each title and each user and assistant entry (its text, then
// its tool calls' names) in which every word begins a word, any case;
// newest first, at most proto.MaxSearchSessionHits of one session's
// entries and the request's limit; the snippet is the text's start, its
// matches marked.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	words, err := proto.SearchWords(r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	limit := proto.DefaultSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > proto.MaxSearchLimit {
			writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, "limit is a whole number from 1 to 200")
			return
		}
		limit = n
	}
	s.mu.Lock()
	indexing := s.searchIndexing
	var titles, entries []proto.SearchHit
	for _, sess := range s.sessions {
		d := sess.detail
		if hit, ok := fakeHit(d.Session.Title, words); ok {
			hit.SessionID, hit.Field, hit.At = d.Session.ID, proto.SearchTitle, d.Session.UpdatedAt
			titles = append(titles, hit)
		}
		for _, e := range d.Entries {
			text, field := e.Text, proto.SearchUser
			switch e.Kind {
			case proto.EntryUser:
			case proto.EntryAssistant:
				field = proto.SearchAssistant
				var names []string
				for _, c := range e.ToolCalls {
					names = append(names, c.Name)
				}
				if len(names) > 0 {
					text = strings.TrimSpace(text + "\nTools: " + strings.Join(names, ", "))
				}
			default:
				continue
			}
			if hit, ok := fakeHit(text, words); ok {
				hit.SessionID, hit.EntryID, hit.Field, hit.At = d.Session.ID, e.ID, field, e.At
				entries = append(entries, hit)
			}
		}
	}
	s.mu.Unlock()
	sort.Slice(entries, func(i, j int) bool { return entries[i].EntryID > entries[j].EntryID })
	cut := false
	per := map[int64]int{}
	kept := entries[:0]
	for _, h := range entries {
		if per[h.SessionID] == proto.MaxSearchSessionHits {
			cut = true
			continue
		}
		per[h.SessionID]++
		kept = append(kept, h)
	}
	hits := append(titles, kept...)
	sort.SliceStable(hits, func(i, j int) bool {
		if !hits[i].At.Equal(hits[j].At) {
			return hits[i].At.After(hits[j].At)
		}
		return hits[i].EntryID < hits[j].EntryID
	})
	if len(hits) > limit {
		hits, cut = hits[:limit], true
	}
	if hits == nil {
		hits = []proto.SearchHit{}
	}
	writeJSON(w, http.StatusOK, proto.SearchResults{Words: words, Hits: hits, Cut: cut, Indexing: indexing})
}

// fakeHit is text as a hit, when every word begins a word of it: its
// first proto.SearchSnippet characters, spaces run together, each match
// marked.
func fakeHit(text string, words []string) (proto.SearchHit, bool) {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	if len(runes) > proto.SearchSnippet {
		runes = append(runes[:proto.SearchSnippet], '…')
	}
	found := map[string]bool{}
	var marks []proto.SearchMark
	offset := 0
	for i := 0; i < len(runes); {
		if !proto.SearchRune(runes[i]) {
			offset += len(string(runes[i]))
			i++
			continue
		}
		j := i
		for j < len(runes) && proto.SearchRune(runes[j]) {
			j++
		}
		longest := 0
		for _, w := range words {
			if begins(runes[i:j], []rune(w)) {
				found[w] = true
				longest = max(longest, len([]rune(w)))
			}
		}
		if longest > 0 {
			n := len(string(runes[i : i+longest]))
			marks = append(marks, proto.SearchMark{Start: offset, End: offset + n})
		}
		offset += len(string(runes[i:j]))
		i = j
	}
	for _, w := range words {
		if !found[w] {
			return proto.SearchHit{}, false
		}
	}
	return proto.SearchHit{Snippet: string(runes), Marks: marks}, true
}

// begins reports whether word, in lower case, begins token, any case.
func begins(token, word []rune) bool {
	if len(word) == 0 || len(word) > len(token) {
		return false
	}
	for k, r := range word {
		if unicode.ToLower(token[k]) != r {
			return false
		}
	}
	return true
}
