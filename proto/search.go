package proto

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// Search finds sessions by their titles and by what their transcripts say
// (ADR-0050, rule 63): GET /v1/search?q=<words>&limit=<n>.
const (
	// MaxSearchQuery is the most bytes of q.
	MaxSearchQuery = 200
	// MaxSearchWords is the most words a search takes, and MaxSearchWord
	// the most characters of one.
	MaxSearchWords = 8
	MaxSearchWord  = 64
	// DefaultSearchLimit is the most hits an answer carries when the
	// request names no limit, MaxSearchLimit the most it may name.
	DefaultSearchLimit = 50
	MaxSearchLimit     = 200
	// MaxSearchSessionHits is the most hits of one session's entries an
	// answer carries: a long session that says a word often leaves room
	// for the others.
	MaxSearchSessionHits = 5
	// SearchSnippet is the most characters of a snippet's text, its
	// ellipses aside.
	SearchSnippet = 160
)

// SearchField is where a hit was found.
type SearchField string

const (
	// SearchTitle: the session's title.
	SearchTitle SearchField = "title"
	// SearchUser: a user entry's text, as the owner's message, a thread's
	// brief, a report or a supervisor's message.
	SearchUser SearchField = "user"
	// SearchAssistant: an agent's reply, its text or the names of its tool
	// calls.
	SearchAssistant SearchField = "assistant"
)

// SearchMark is one match a snippet marks: the bytes of Snippet from Start
// to End, a word's start that one of the words searched begins.
type SearchMark struct {
	Start int `json:"start"`
	End   int `json:"end"`
}

// SearchHit is one place the words were found: a session's title (Entry
// 0), or one entry of its transcript.
type SearchHit struct {
	SessionID int64 `json:"sessionId"`
	// EntryID is the entry found, 0 for the title's hit.
	EntryID int64       `json:"entryId,omitempty"`
	Field   SearchField `json:"field"`
	// At is the entry's time, absent for an entry PiG gave none; for the
	// title's hit, the session's updatedAt.
	At time.Time `json:"at,omitzero"`
	// Snippet is the text around the first match, at most SearchSnippet
	// characters with "…" where it is cut, and Marks the matches in it,
	// in order.
	Snippet string       `json:"snippet"`
	Marks   []SearchMark `json:"marks,omitempty"`
}

// SearchResults is the answer of GET /v1/search.
type SearchResults struct {
	// Words are the words searched, as SearchWords reads q.
	Words []string `json:"words"`
	// Hits are newest first: at most the request's limit, and at most
	// MaxSearchSessionHits of one session's entries.
	Hits []SearchHit `json:"hits"`
	// Cut says hits were left out: past the limit, or past a session's
	// MaxSearchSessionHits.
	Cut bool `json:"cut,omitempty"`
	// Indexing says the service still reads older entries into its index,
	// as on its first start with search: a hit may be missing.
	Indexing bool `json:"indexing,omitempty"`
}

// ErrNoSearchWords is a query that holds no word to search.
var ErrNoSearchWords = errors.New("give at least one word to search")

// SearchWords are the words of query q, as a search reads them: each run
// of letters and digits, in lower case, once, in the order given. A word
// matches a word of a title or an entry that it begins, whatever its case.
// A query over MaxSearchQuery bytes, with more than MaxSearchWords words
// or a word over MaxSearchWord characters, is refused, and so is one with
// no word (ErrNoSearchWords).
func SearchWords(q string) ([]string, error) {
	if len(q) > MaxSearchQuery {
		return nil, fmt.Errorf("a search has at most %d bytes", MaxSearchQuery)
	}
	if !utf8.ValidString(q) {
		return nil, errors.New("a search is UTF-8 text")
	}
	var words []string
	seen := map[string]bool{}
	var word strings.Builder
	runes := 0
	flush := func() error {
		if word.Len() == 0 {
			return nil
		}
		w := word.String()
		word.Reset()
		if runes > MaxSearchWord {
			return fmt.Errorf("a word to search has at most %d characters", MaxSearchWord)
		}
		runes = 0
		if !seen[w] {
			seen[w] = true
			words = append(words, w)
		}
		return nil
	}
	for _, r := range q {
		if SearchRune(r) {
			word.WriteRune(unicode.ToLower(r))
			runes++
			continue
		}
		if err := flush(); err != nil {
			return nil, err
		}
	}
	if err := flush(); err != nil {
		return nil, err
	}
	switch {
	case len(words) == 0:
		return nil, ErrNoSearchWords
	case len(words) > MaxSearchWords:
		return nil, fmt.Errorf("a search has at most %d words", MaxSearchWords)
	}
	return words, nil
}

// SearchRune reports whether r is part of a word, as a search splits text
// into words: a letter, a number, a private-use character or a mark that
// takes no room of its own (Mn, as an accent written apart), as SQLite's
// unicode61 tokenizer reads them; every other character splits words.
func SearchRune(r rune) bool {
	return unicode.IsLetter(r) || unicode.IsNumber(r) || unicode.Is(unicode.Co, r) || unicode.Is(unicode.Mn, r)
}
