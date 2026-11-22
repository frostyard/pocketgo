package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/search"
)

// The owner's search of every session (ADR-0050): an FTS5 index of what a
// search reads of each entry, fed from PiG's transcripts by
// search.Indexer, and each session's cursor.

// SearchCursors are, by session, the newest entry read into the index.
func (s *Store) SearchCursors(ctx context.Context) (map[int64]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session, through FROM search_cursors`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	out := map[int64]int64{}
	for rows.Next() {
		var session, through int64
		if err := rows.Scan(&session, &through); err != nil {
			return nil, err
		}
		out[session] = through
	}
	return out, rows.Err()
}

// IndexEntries keeps docs of session and moves its cursor to through, in
// one transaction: a doc at or before the cursor it finds is kept already
// and is left as it is, and the cursor never moves back.
func (s *Store) IndexEntries(ctx context.Context, session, through int64, docs []search.Doc) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var cursor int64
	err = tx.QueryRowContext(ctx, `SELECT through FROM search_cursors WHERE session = ?`, session).Scan(&cursor)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	for _, d := range docs {
		if d.Entry <= cursor {
			continue
		}
		var at int64
		if !d.At.IsZero() {
			at = d.At.UnixMilli()
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO search_entries (rowid, text, session, field, at) VALUES (?, ?, ?, ?, ?)`,
			d.Entry, d.Text, session, string(d.Field), at); err != nil {
			return fmt.Errorf("index entry %d: %w", d.Entry, err)
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO search_cursors (session, through) VALUES (?, ?)
		ON CONFLICT (session) DO UPDATE SET through = MAX(through, excluded.through)`, session, through); err != nil {
		return err
	}
	return tx.Commit()
}

// EntryHit is an entry the index found, with the text it holds of it.
type EntryHit struct {
	Session, Entry int64
	Field          proto.SearchField
	// At is the entry's time, zero for none.
	At   time.Time
	Text string
}

// SearchScan is the most matches a search reads, newest first, to choose
// its hits: past them it says hits were left out, rather than read on
// through every line of a long session that says the word.
const SearchScan = 20000

// SearchEntries are the entries that match, the index's query
// (search.Match), newest first by entry id: at most perSession of one
// session, at most limit in all, chosen from the SearchScan newest
// matches. cut says the index holds a match left out.
func (s *Store) SearchEntries(ctx context.Context, match string, perSession, limit int) (hits []EntryHit, cut bool, err error) {
	ids, cut, err := s.searchIDs(ctx, match, perSession, limit)
	if err != nil || len(ids) == 0 {
		return nil, cut, err
	}
	args := make([]any, len(ids))
	marks := make([]string, len(ids))
	for i, id := range ids {
		args[i], marks[i] = id, "?"
	}
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, session, field, at, text FROM search_entries
		WHERE rowid IN (`+strings.Join(marks, ", ")+`) ORDER BY rowid DESC`, args...)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var h EntryHit
		var field string
		var at int64
		if err := rows.Scan(&h.Entry, &h.Session, &field, &at, &h.Text); err != nil {
			return nil, false, err
		}
		h.Field = proto.SearchField(field)
		if at != 0 {
			h.At = fromMillis(at)
		}
		hits = append(hits, h)
	}
	return hits, cut, rows.Err()
}

// searchIDs reads the matches newest first, which the index gives in its
// own order with no sort, until it has limit of them, at most perSession
// a session, or has read SearchScan.
func (s *Store) searchIDs(ctx context.Context, match string, perSession, limit int) (ids []int64, cut bool, err error) {
	rows, err := s.db.QueryContext(ctx, `SELECT rowid, session FROM search_entries WHERE search_entries MATCH ? ORDER BY rowid DESC`, match)
	if err != nil {
		return nil, false, err
	}
	defer func() { _ = rows.Close() }()
	per := map[int64]int{}
	for read := 0; rows.Next(); read++ {
		if len(ids) == limit || read == SearchScan {
			return ids, true, nil
		}
		var id, session int64
		if err := rows.Scan(&id, &session); err != nil {
			return nil, false, err
		}
		if per[session] == perSession {
			cut = true
			continue
		}
		per[session]++
		ids = append(ids, id)
	}
	return ids, cut, rows.Err()
}
