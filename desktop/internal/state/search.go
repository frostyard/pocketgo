package state

import (
	"context"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// The owner's search of every session (ADR-0050): the sidebar's filter
// offers it, the Search view shows its hits grouped by session, and a hit
// opens its session at its entry. pocketd searches (GET /v1/search); these
// are the client's rules about it.

// PlaceSearch is the Search view: on a compact window, it shows over the
// Sessions tab, from whose field it opens (TabOf).
const PlaceSearch = "search"

// searchState is the newest search: the query of the fetch begun last, and
// the answer of the newest fetch that answered.
type searchState struct {
	asked   string // the query of the fetch begun last
	begun   uint64 // its epoch
	query   string // the query the results answer
	results proto.SearchResults
	list    list
}

// Search is the search as the Search view shows it.
type Search struct {
	// Query is the query the newest search asked, Answered the one its
	// Results answer: they differ while a newer one is in flight.
	Query, Answered string
	Results         proto.SearchResults
	// Status is where the answered search stands, and Problem why it
	// failed; Loading says the newest search has not answered yet.
	Status  ListStatus
	Problem string
	Loading bool
}

// Search is the newest search.
func (d *Data) Search() Search {
	s := &d.search
	return Search{Query: s.asked, Answered: s.query, Results: s.results, Status: s.list.status, Problem: s.list.err,
		Loading: s.begun != 0 && s.list.applied < s.begun}
}

// BeginSearch starts a search of q: the answer of a search begun later
// wins over its own.
func (d *Data) BeginSearch(q string) Fetch {
	f := d.Begin()
	d.rev++
	d.search.asked, d.search.begun = q, f.epoch
	return f
}

// SetSearch records search f's answer to q, unless a search begun later
// already answered.
func (d *Data) SetSearch(f Fetch, q string, r proto.SearchResults) {
	if !d.search.list.answer(f) {
		return
	}
	d.rev++
	d.search.list.status, d.search.list.err = ListLoaded, ""
	r.Hits = append([]proto.SearchHit(nil), r.Hits...)
	d.search.query, d.search.results = q, r
}

// SetSearchError records that search f of q failed, unless a search begun
// later already answered; it shows no hits of another query.
func (d *Data) SetSearchError(f Fetch, q, err string) {
	if !d.search.list.answer(f) {
		return
	}
	d.rev++
	d.search.list.status, d.search.list.err = ListFailed, err
	d.search.query, d.search.results = q, proto.SearchResults{}
}

// Search asks pocketd for the sessions whose titles or transcripts hold
// the words of q (GET /v1/search). A query pocketd would refuse
// (proto.SearchWords) is refused here, with no request.
func (s *Store) Search(q string) {
	f := s.Data.BeginSearch(q)
	if _, err := proto.SearchWords(q); err != nil {
		s.Data.SetSearchError(f, q, err.Error())
		return
	}
	var out proto.SearchResults
	s.do("Searching the sessions", func(ctx context.Context, c *proto.Client) (err error) {
		out, err = c.Search(ctx, q, 0)
		return err
	}, func(err error) {
		if err != nil {
			s.Data.SetSearchError(f, q, err.Error())
			return
		}
		s.Data.SetSearch(f, q, out)
	})
}

// searchOfferMost is the most characters of the filter the sidebar's
// search row quotes.
const searchOfferMost = 40

// SearchOffer is the sidebar's last row under a filter: "Search all
// sessions for “filter”", when the filter holds a word to search
// (proto.SearchWords); false when it holds none, or more than a search
// takes. A long filter is quoted cut, with "…".
func SearchOffer(filter string) (string, bool) {
	q := strings.TrimSpace(filter)
	if _, err := proto.SearchWords(q); err != nil {
		return "", false
	}
	if utf8.RuneCountInString(q) > searchOfferMost {
		q = string([]rune(q)[:searchOfferMost]) + "…"
	}
	return "Search all sessions for “" + q + "”", true
}

// SearchGroup is a session's hits, as the Search view shows them under
// its title.
type SearchGroup struct {
	Session int64
	// Title is the session's title as the sidebar shows it, "Session N"
	// for one the client does not hold, and Tag its agent's name or
	// folder, as its sidebar row says.
	Title, Tag string
	Hits       []proto.SearchHit
}

// SearchGroups are the newest search's hits grouped by session: the
// sessions in the order of their newest hit, each hit in pocketd's order,
// newest first.
func (d *Data) SearchGroups() []SearchGroup {
	var groups []SearchGroup
	at := map[int64]int{}
	for _, h := range d.search.results.Hits {
		i, ok := at[h.SessionID]
		if !ok {
			i = len(groups)
			at[h.SessionID] = i
			g := SearchGroup{Session: h.SessionID, Title: "Session " + strconv.FormatInt(h.SessionID, 10)}
			if s, held := d.Session(h.SessionID); held {
				g.Title, g.Tag = SessionTitle(s), d.sessionTag(s, s.Profile)
			}
			groups = append(groups, g)
		}
		groups[i].Hits = append(groups[i].Hits, h)
	}
	return groups
}

// SearchNote is the line over the Search view's hits: how many hits in how
// many sessions, and what may be missing from them.
func SearchNote(r proto.SearchResults, groups int) string {
	if len(r.Hits) == 0 {
		return ""
	}
	note := plural(len(r.Hits), "hit", "hits") + " in " + plural(groups, "session", "sessions")
	if r.Cut {
		note += ", the newest: more words find older ones"
	}
	return note
}

// IndexingNote says that pocketd still reads older sessions into its
// search, when it does: a session may be missing.
func IndexingNote(r proto.SearchResults) string {
	if !r.Indexing {
		return ""
	}
	return "pocketd is still reading older sessions into its search: some may be missing."
}

// HitField names where a hit was found, as its caption says.
func HitField(f proto.SearchField) string {
	switch f {
	case proto.SearchTitle:
		return "Title"
	case proto.SearchUser:
		return "Message"
	case proto.SearchAssistant:
		return "Reply"
	}
	return string(f)
}

// Span is a run of a snippet: Marked for a match of the words searched.
type Span struct {
	Text   string
	Marked bool
}

// SnippetSpans are snippet's runs, its marks as matches: only the marks
// that lie in it whole, on characters' bounds, in order and apart, and
// none that is empty; a mark that breaks a rule is text.
func SnippetSpans(snippet string, marks []proto.SearchMark) []Span {
	var spans []Span
	at := 0
	for _, m := range marks {
		if m.Start < at || m.End <= m.Start || m.End > len(snippet) ||
			!utf8.RuneStart(snippet[m.Start]) || (m.End < len(snippet) && !utf8.RuneStart(snippet[m.End])) {
			continue
		}
		if m.Start > at {
			spans = append(spans, Span{Text: snippet[at:m.Start]})
		}
		spans = append(spans, Span{Text: snippet[m.Start:m.End], Marked: true})
		at = m.End
	}
	if at < len(snippet) || len(spans) == 0 {
		spans = append(spans, Span{Text: snippet[at:]})
	}
	return spans
}

// TargetRow is the transcript row a hit of entry opens at: the first row
// built from it, else the first built from an entry after it, as for an
// entry that shows no row of its own; false for none, or for entry 0, a
// title's hit, which opens the session where it opens.
func TargetRow(rows []Row, entry int64) (int, bool) {
	if entry == 0 {
		return 0, false
	}
	after := -1
	for i, r := range rows {
		switch {
		case r.Entry == entry:
			return i, true
		case r.Entry > entry && after < 0:
			after = i
		}
	}
	return after, after >= 0
}

// Seek is a hit the owner opened: its session shows, scrolled to its entry
// once its transcript loaded.
type Seek struct {
	Session, Entry int64
}

// Target is where seek scrolls the open session o, built as rows: row, once
// o is seek's session and has loaded (done); found is false when no row
// stands for the entry, and the session shows where it opens. Before it
// loaded, done is false and the seek waits.
func (s Seek) Target(o *OpenSession, rows []Row) (row int, found, done bool) {
	if o == nil || o.ID != s.Session {
		return 0, false, true
	}
	if !o.Loaded {
		return 0, false, false
	}
	row, found = TargetRow(rows, s.Entry)
	return row, found, true
}
