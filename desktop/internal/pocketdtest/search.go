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
// entries chosen newest first by their times, the later entry first at
// one time, at most proto.MaxSearchSessionHits of one session's and the
// request's limit, which takes pocketd's whole numbers alone; the hits
// ordered as pocketd's search.Arrange orders them; the snippet is the
// text around the first match, its matches marked (Codex review of #111,
// round 1).
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	words, err := proto.SearchWords(r.URL.Query().Get("q"))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	limit := proto.DefaultSearchLimit
	if raw := r.URL.Query().Get("limit"); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 1 || n > proto.MaxSearchLimit || strconv.Itoa(n) != raw {
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
	sort.SliceStable(entries, func(i, j int) bool { return hitBefore(entries[i], entries[j]) })
	cut := false
	per := map[int64]int{}
	var kept []proto.SearchHit
	for _, h := range entries {
		if len(kept) == limit {
			cut = true
			break
		}
		if per[h.SessionID] == proto.MaxSearchSessionHits {
			cut = true
			continue
		}
		per[h.SessionID]++
		kept = append(kept, h)
	}
	hits := append(titles, kept...)
	sort.SliceStable(hits, func(i, j int) bool { return hitBefore(hits[i], hits[j]) })
	if len(hits) > limit {
		hits, cut = hits[:limit], true
	}
	if hits == nil {
		hits = []proto.SearchHit{}
	}
	writeJSON(w, http.StatusOK, proto.SearchResults{Words: words, Hits: hits, Cut: cut, Indexing: indexing})
}

// hitBefore orders hits as pocketd's search.Arrange does: newest first, a
// title's before an entry's at one time, then the later entry, then the
// later session.
func hitBefore(a, b proto.SearchHit) bool {
	if !a.At.Equal(b.At) {
		return a.At.After(b.At)
	}
	if (a.EntryID == 0) != (b.EntryID == 0) {
		return a.EntryID == 0
	}
	if a.EntryID != b.EntryID {
		return a.EntryID > b.EntryID
	}
	return a.SessionID > b.SessionID
}

// fakeHit is text as a hit, when every word begins a word of it, anywhere
// in it: proto.SearchSnippet characters from 40 before its first match,
// spaces run together, with "…" where text goes on, each match in them
// marked.
func fakeHit(text string, words []string) (proto.SearchHit, bool) {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	type match struct{ start, end int }
	var found []match
	seen := map[string]bool{}
	for i := 0; i < len(runes); {
		if !proto.SearchRune(runes[i]) {
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
				seen[w] = true
				longest = max(longest, len([]rune(w)))
			}
		}
		if longest > 0 {
			found = append(found, match{i, i + longest})
		}
		i = j
	}
	for _, w := range words {
		if !seen[w] {
			return proto.SearchHit{}, false
		}
	}
	start := 0
	if len(found) > 0 && found[0].start > 40 {
		start = found[0].start - 40
	}
	end := min(len(runes), start+proto.SearchSnippet)
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	var marks []proto.SearchMark
	at := start
	for _, m := range found {
		if m.start < start || m.end > end {
			continue
		}
		b.WriteString(string(runes[at:m.start]))
		mark := proto.SearchMark{Start: b.Len()}
		b.WriteString(string(runes[m.start:m.end]))
		mark.End = b.Len()
		marks = append(marks, mark)
		at = m.end
	}
	b.WriteString(string(runes[at:end]))
	if end < len(runes) {
		b.WriteString("…")
	}
	return proto.SearchHit{Snippet: b.String(), Marks: marks}, true
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
