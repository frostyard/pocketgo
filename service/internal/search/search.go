// Package search holds the rules of the owner's search of every session
// (ADR-0050, protocol rule 63): what of an entry the index holds, the
// query the index runs, which titles a search finds, the snippet of a hit
// with its matches marked, the limits of an answer and its order. The
// work database keeps the index (store.SearchEntries); Indexer feeds it
// from the transcripts PiG keeps.
package search

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// MaxIndexed is the most bytes of an entry's text the index holds: words
// past it are not found.
const MaxIndexed = 32 << 10

// snippetLead is how many characters of text before its first match a
// snippet shows, at most.
const snippetLead = 40

// Doc is one entry as the index holds it.
type Doc struct {
	Entry int64
	Field proto.SearchField
	At    time.Time
	Text  string
}

// DocOf is entry e as the index holds it: a user entry's text (the owner's
// message, a thread's brief, a report, an answer or a supervisor's
// message, as pocketd placed it), and an agent's reply, its text then the
// names of its tool calls, cut to MaxIndexed bytes at a character. Nothing
// else is searched: no tool's output or arguments, no thinking, no system,
// compaction or reset entry. False for an entry with nothing to search.
func DocOf(e proto.Entry) (Doc, bool) {
	var text string
	var field proto.SearchField
	switch e.Kind {
	case proto.EntryUser:
		text, field = e.Text, proto.SearchUser
	case proto.EntryAssistant:
		field = proto.SearchAssistant
		text = strings.TrimSpace(e.Text)
		var names []string
		for _, call := range e.ToolCalls {
			if name := strings.TrimSpace(call.Name); name != "" {
				names = append(names, name)
			}
		}
		if len(names) > 0 {
			if text != "" {
				text += "\n"
			}
			text += "Tools: " + strings.Join(names, ", ")
		}
	default:
		return Doc{}, false
	}
	text = cut(strings.TrimSpace(text), MaxIndexed)
	if text == "" {
		return Doc{}, false
	}
	return Doc{Entry: e.ID, Field: field, At: e.At, Text: text}, true
}

// cut is text in at most n bytes, cut at a character.
func cut(text string, n int) string {
	if len(text) <= n {
		return text
	}
	for n > 0 && !utf8.RuneStart(text[n]) {
		n--
	}
	return text[:n]
}

// Match is the index's query for words, as proto.SearchWords reads them:
// every word begins a word of the entry ("w"* for each, all of them).
func Match(words []string) string {
	parts := make([]string, len(words))
	for i, w := range words {
		parts[i] = `"` + strings.ReplaceAll(w, `"`, `""`) + `"*`
	}
	return strings.Join(parts, " AND ")
}

// ErrLimit is a limit that is not a whole number from 1 to
// proto.MaxSearchLimit.
var ErrLimit = fmt.Errorf("limit is a whole number from 1 to %d", proto.MaxSearchLimit)

// Limit is the most hits an answer carries, as the request's limit says:
// proto.DefaultSearchLimit for none.
func Limit(raw string) (int, error) {
	if raw == "" {
		return proto.DefaultSearchLimit, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n < 1 || n > proto.MaxSearchLimit || strconv.Itoa(n) != raw {
		return 0, ErrLimit
	}
	return n, nil
}

// Holds reports whether every word begins a word of text, whatever its
// case, as the index matches an entry: a session's title is matched so.
// Every word that begins a word of text counts, not only the longest at
// each: "dep deploy" finds "Deployment" as the index does (Codex review of
// #111, round 1).
func Holds(text string, words []string) bool {
	runes := []rune(text)
	lowered := make([][]rune, len(words))
	for i, w := range words {
		lowered[i] = []rune(w)
	}
	found := map[int]bool{}
	for i := 0; i < len(runes); {
		if !proto.SearchRune(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && proto.SearchRune(runes[j]) {
			j++
		}
		for k, w := range lowered {
			if len(w) > 0 && len(w) <= j-i && begins(runes[i:j], w) {
				found[k] = true
			}
		}
		i = j
	}
	return len(found) == len(words)
}

// match is where a word of the words searched begins a word of a text, in
// characters, and which.
type match struct {
	start, end int
	word       int
}

// matches are the matches of words in runes, in order: at each start of a
// word of the text, the longest word searched that begins it.
func matches(runes []rune, words []string) []match {
	lowered := make([][]rune, len(words))
	for i, w := range words {
		lowered[i] = []rune(w)
	}
	var out []match
	for i := 0; i < len(runes); {
		if !proto.SearchRune(runes[i]) {
			i++
			continue
		}
		j := i
		for j < len(runes) && proto.SearchRune(runes[j]) {
			j++
		}
		best := -1
		for k, w := range lowered {
			if len(w) == 0 || len(w) > j-i || (best >= 0 && len(w) <= len(lowered[best])) {
				continue
			}
			if begins(runes[i:j], w) {
				best = k
			}
		}
		if best >= 0 {
			out = append(out, match{start: i, end: i + len(lowered[best]), word: best})
		}
		i = j
	}
	return out
}

// begins reports whether word, in lower case, begins token, whatever its
// case.
func begins(token, word []rune) bool {
	for k, r := range word {
		if unicode.ToLower(token[k]) != r {
			return false
		}
	}
	return true
}

// Snippet is the text around the first match of words in text, and the
// matches in it: its spaces run together as one, at most
// proto.SearchSnippet characters from a word's start at most snippetLead
// characters before the first match, cut after a word where it can, with
// "…" where text goes on. The marks are byte ranges of the snippet, in
// order. A text in which no word is found shows from its start, unmarked:
// the index may fold a letter's case as Go does not.
func Snippet(text string, words []string) (string, []proto.SearchMark) {
	runes := []rune(strings.Join(strings.Fields(text), " "))
	found := matches(runes, words)
	start := 0
	if len(found) > 0 && found[0].start > snippetLead {
		first := found[0].start
		start = first - snippetLead
		for start < first && runes[start-1] != ' ' {
			start++
		}
	}
	end := len(runes)
	if end-start > proto.SearchSnippet {
		end = start + proto.SearchSnippet
		least := start
		if len(found) > 0 && found[0].end <= end {
			least = found[0].end
		}
		for at := end; at > least; at-- {
			if runes[at] == ' ' {
				end = at
				break
			}
		}
	}
	var b strings.Builder
	if start > 0 {
		b.WriteString("…")
	}
	var marks []proto.SearchMark
	offset := b.Len()
	at := start
	for _, m := range found {
		if m.start < start || m.end > end {
			continue
		}
		offset += len(string(runes[at:m.start]))
		mark := proto.SearchMark{Start: offset}
		offset += len(string(runes[m.start:m.end]))
		mark.End = offset
		marks = append(marks, mark)
		at = m.end
	}
	b.WriteString(strings.TrimRight(string(runes[start:end]), " "))
	if end < len(runes) {
		b.WriteString("…")
	}
	return b.String(), marks
}

// Arrange is an answer's hits: titles' hits and entries' hits, newest
// first by their times, the newer entry first at one time and a title's
// hit before an entry's, at most limit of them; cut says it left one out.
// entries come from the index already at most proto.MaxSearchSessionHits
// a session.
func Arrange(titles, entries []proto.SearchHit, limit int) (hits []proto.SearchHit, cut bool) {
	hits = append(append(make([]proto.SearchHit, 0, len(titles)+len(entries)), titles...), entries...)
	sort.SliceStable(hits, func(i, j int) bool {
		a, b := hits[i], hits[j]
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
	})
	if len(hits) > limit {
		return hits[:limit], true
	}
	return hits, false
}
