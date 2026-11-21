package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// UploadRow is a file the owner uploaded to a session (ADR-0029): File is
// its name in the session's uploads folder, Name its safe name, Mime its
// type and Size its bytes, as uploaded.
type UploadRow struct {
	ID        int64
	Session   int64
	File      string
	Name      string
	Mime      string
	Size      int64
	CreatedAt time.Time
}

// RecordUpload records row, session's new upload, with the request of key
// and kind that stored it, in one transaction: both are kept or neither
// is. A key recorded before for kind answers the upload it recorded, with
// existed set, and records nothing; a key recorded for another kind is
// ErrRequestReused.
func (s *Store) RecordUpload(ctx context.Context, key, kind string, row UploadRow) (out UploadRow, existed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadRow{}, false, err
	}
	defer func() { _ = tx.Rollback() }()
	result, existed, err := claimRequest(ctx, tx, key, kind, now)
	if err != nil {
		return UploadRow{}, existed, err
	}
	if existed {
		id, err := strconv.ParseInt(result, 10, 64)
		if err != nil {
			return UploadRow{}, true, fmt.Errorf("request %s records no upload (%q)", kind, result)
		}
		out, err := scanUpload(tx.QueryRowContext(ctx, uploadColumns+` WHERE id = ?`, id).Scan)
		return out, true, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO uploads (session, file, name, mime, size, request, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		row.Session, row.File, row.Name, row.Mime, row.Size, key, now)
	if err != nil {
		return UploadRow{}, false, err
	}
	if row.ID, err = res.LastInsertId(); err != nil {
		return UploadRow{}, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, strconv.FormatInt(row.ID, 10), key); err != nil {
		return UploadRow{}, false, err
	}
	row.CreatedAt = fromMillis(now)
	return row, false, tx.Commit()
}

const uploadColumns = `SELECT id, session, file, name, mime, size, created_at FROM uploads`

func scanUpload(scan func(...any) error) (UploadRow, error) {
	var row UploadRow
	var created int64
	err := scan(&row.ID, &row.Session, &row.File, &row.Name, &row.Mime, &row.Size, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadRow{}, ErrNotFound
	}
	row.CreatedAt = fromMillis(created)
	return row, err
}

// Upload is upload id.
func (s *Store) Upload(ctx context.Context, id int64) (UploadRow, error) {
	return scanUpload(s.db.QueryRowContext(ctx, uploadColumns+` WHERE id = ?`, id).Scan)
}

// ErrNotSessions is an upload id that names no upload of the session.
var ErrNotSessions = errors.New("no upload of the session")

// SessionUploads are session's uploads ids, in their order, an id named
// twice once. An id that is no upload of session is ErrNotSessions, with
// a message that names it.
func (s *Store) SessionUploads(ctx context.Context, session int64, ids []int64) ([]UploadRow, error) {
	var out []UploadRow
	seen := map[int64]bool{}
	for _, id := range ids {
		if seen[id] {
			continue
		}
		seen[id] = true
		row, err := s.Upload(ctx, id)
		if errors.Is(err, ErrNotFound) || err == nil && row.Session != session {
			return nil, fmt.Errorf("%w: %d is no upload of session %d", ErrNotSessions, id, session)
		} else if err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, nil
}
