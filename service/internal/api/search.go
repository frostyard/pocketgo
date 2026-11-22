package api

import (
	"context"
	"net/http"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/search"
)

// The owner's search of every session (ADR-0050, rule 63): the titles in
// the work database, and the entries in its index, which search.Indexer
// keeps up with PiG's transcripts.

// engineEntries is the indexer's Source: the engine, as it is wired when
// the indexer reads (SetEngine comes after New).
type engineEntries struct{ s *Server }

func (e engineEntries) EntriesAfter(ctx context.Context, session, after int64, most int) ([]proto.Entry, error) {
	return e.s.engine.EntriesAfter(ctx, session, after, most)
}

// StartIndex reads every session's entries after its cursor into the
// search index, and then each session an entry's event notes, until Stop.
// Serving does not wait for it: it only reads PiG, and submits nothing.
func (s *Server) StartIndex(ctx context.Context) {
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		// The sessions an entry notes are still read; the others wait for
		// the next start.
		s.log.Warn("list the sessions to index", "err", err)
	}
	ids := make([]int64, len(rows))
	for i, row := range rows {
		ids[i] = row.ID
	}
	s.index.Start(ids)
}

// search answers GET /v1/search: the sessions whose titles hold the words,
// and the entries that do, newest first, within the request's limit and
// proto.MaxSearchSessionHits a session (rule 63). It changes nothing.
func (s *Server) search(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	words, err := proto.SearchWords(query.Get("q"))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	limit, err := search.Limit(query.Get("limit"))
	if err != nil {
		writeError(w, http.StatusBadRequest, proto.ErrorCodeBadRequest, err.Error())
		return
	}
	ctx := r.Context()
	rows, err := s.store.Sessions(ctx)
	if err != nil {
		s.internal(w, "list the sessions", err)
		return
	}
	var titles []proto.SearchHit
	known := make(map[int64]bool, len(rows))
	for _, row := range rows {
		known[row.ID] = true
		if search.Holds(row.Title, words) {
			snippet, marks := search.Snippet(row.Title, words)
			titles = append(titles, proto.SearchHit{SessionID: row.ID, Field: proto.SearchTitle, At: row.UpdatedAt, Snippet: snippet, Marks: marks})
		}
	}
	found, cut, err := s.store.SearchEntries(ctx, search.Match(words), proto.MaxSearchSessionHits, limit)
	if err != nil {
		s.internal(w, "search the entries", err)
		return
	}
	var entries []proto.SearchHit
	for _, h := range found {
		// The index may hold a session the work database does not list,
		// as one restored apart from the other: its hit would open
		// nothing.
		if !known[h.Session] {
			continue
		}
		snippet, marks := search.Snippet(h.Text, words)
		entries = append(entries, proto.SearchHit{SessionID: h.Session, EntryID: h.Entry, Field: h.Field, At: h.At, Snippet: snippet, Marks: marks})
	}
	hits, more := search.Arrange(titles, entries, limit)
	writeJSON(w, http.StatusOK, proto.SearchResults{Words: words, Hits: hits, Cut: cut || more, Indexing: s.index.Behind()})
}
