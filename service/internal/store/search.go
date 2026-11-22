package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/search"
)

// The owner's search of every session (ADR-0050): an FTS5 index of what a
// search reads of each entry, fed from PiG's transcripts by
// search.Indexer, and each session's cursor.

// SearchSessions are every session's id, the most recently updated
// first: the indexer's start reads them in that order.
func (s *Store) SearchSessions(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM sessions ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

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

// SearchScan is the most matches a search reads, the last written first,
// to choose its hits: past them it says hits were left out, rather than
// read on through every line of a long session that says the word.
const SearchScan = 20000

// SearchEntries are the entries that match, the index's query
// (search.Match), newest first by their time, the later entry first at one
// time and an entry with no time last: at most perSession of one session,
// at most limit in all, chosen from the SearchScan matches last written.
// cut says the index holds a match left out.
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
	if err := rows.Err(); err != nil {
		return nil, false, err
	}
	sort.Slice(hits, func(i, j int) bool {
		return newer(hits[i].At.UnixMilli(), hits[i].Entry, hits[j].At.UnixMilli(), hits[j].Entry)
	})
	return hits, cut, nil
}

// newer reports whether the entry a, at time at (Unix milliseconds, 0 for
// none), comes before entry b, at bt, newest first.
func newer(at, a, bt, b int64) bool {
	if at != bt {
		return at > bt
	}
	return a > b
}

// searchIDs reads the SearchScan matches last written, which the index
// gives in its own order with no sort, then chooses newest first by their
// times, as rule 63 orders hits, at most perSession a session and limit in
// all: an entry's time is its message's, which a message queued before
// another was written may have earlier (Codex review of #111, round 1).
func (s *Store) searchIDs(ctx context.Context, match string, perSession, limit int) (ids []int64, cut bool, err error) {
	type found struct{ id, session, at int64 }
	var all []found
	err = func() error {
		rows, err := s.db.QueryContext(ctx, `SELECT rowid, session, at FROM search_entries WHERE search_entries MATCH ? ORDER BY rowid DESC`, match)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			if len(all) == SearchScan {
				cut = true
				break
			}
			var f found
			if err := rows.Scan(&f.id, &f.session, &f.at); err != nil {
				return err
			}
			all = append(all, f)
		}
		return rows.Err()
	}()
	if err != nil {
		return nil, false, err
	}
	sort.Slice(all, func(i, j int) bool { return newer(all[i].at, all[i].id, all[j].at, all[j].id) })
	per := map[int64]int{}
	for i, f := range all {
		if len(ids) == limit {
			return ids, true, nil
		}
		if per[f.session] == perSession {
			cut = true
			continue
		}
		per[f.session]++
		ids = append(ids, all[i].id)
	}
	return ids, cut, nil
}
