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

// An upload is claimed before its file is written, pending, and stored
// once it is: a stop of pocketd between the two leaves a pending upload,
// whose file, whole or cut, the next start removes with its claim (Codex
// review of #49, round 1). Only a stored upload is an upload.
const (
	uploadPending = "pending"
	uploadStored  = "stored"
)

// ClaimUpload claims key, of kind, for row, session's new upload, pending
// until FinishUpload stores it: the request's claim and the pending row go
// in one transaction. A key recorded before for kind answers its upload,
// with existed set: stored when its request has a result, else the
// pending one a try left. A key recorded for another kind is
// ErrRequestReused.
func (s *Store) ClaimUpload(ctx context.Context, key, kind string, row UploadRow) (out UploadRow, pending, existed bool, err error) {
	now := s.millis()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadRow{}, false, false, err
	}
	defer func() { _ = tx.Rollback() }()
	_, existed, err = claimRequest(ctx, tx, key, kind, now)
	if err != nil {
		return UploadRow{}, false, existed, err
	}
	if existed {
		var state string
		out, err = scanUpload(tx.QueryRowContext(ctx, `SELECT id, session, file, name, mime, size, created_at, state FROM uploads WHERE request = ?`, key).Scan, &state)
		return out, state == uploadPending, true, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO uploads (session, file, name, mime, size, request, created_at, state) VALUES (?, ?, ?, '', 0, ?, ?, ?)`,
		row.Session, row.File, row.Name, key, now, uploadPending)
	if err != nil {
		return UploadRow{}, false, false, err
	}
	if row.ID, err = res.LastInsertId(); err != nil {
		return UploadRow{}, false, false, err
	}
	row.CreatedAt = fromMillis(now)
	return row, true, false, tx.Commit()
}

// FinishUpload stores key's pending upload, of size bytes and type mime,
// and completes its request, in one transaction.
func (s *Store) FinishUpload(ctx context.Context, key string, size int64, mime string) (UploadRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	row, err := scanUpload(tx.QueryRowContext(ctx, `SELECT id, session, file, name, mime, size, created_at, state FROM uploads WHERE request = ?`, key).Scan, &state)
	if err != nil {
		return UploadRow{}, err
	}
	if state != uploadPending {
		return UploadRow{}, fmt.Errorf("upload %d is %s, not pending: %w", row.ID, state, ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE uploads SET state = ?, size = ?, mime = ? WHERE id = ?`, uploadStored, size, mime, row.ID); err != nil {
		return UploadRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, strconv.FormatInt(row.ID, 10), key); err != nil {
		return UploadRow{}, err
	}
	row.Size, row.Mime = size, mime
	return row, tx.Commit()
}

// DropUpload removes key's pending upload and frees its claim, in one
// transaction, once its file is gone: a request that failed leaves no
// record. A stored upload, and its request, stay.
func (s *Store) DropUpload(ctx context.Context, key string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE request = ? AND state = ?`, key, uploadPending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE key = ? AND result = ''`, key); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingUpload is an upload claimed and not stored: its request's key,
// and its file.
type PendingUpload struct {
	Key     string
	Session int64
	File    string
}

// PendingUploads are the uploads claimed before before and never stored:
// a stop of pocketd left them, and no request of this run writes them.
func (s *Store) PendingUploads(ctx context.Context, before time.Time) ([]PendingUpload, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT request, session, file FROM uploads WHERE state = ? AND created_at < ? ORDER BY id`, uploadPending, before.UnixMilli())
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PendingUpload
	for rows.Next() {
		var p PendingUpload
		if err := rows.Scan(&p.Key, &p.Session, &p.File); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingUploadOf is key's pending upload, if it has one.
func (s *Store) PendingUploadOf(ctx context.Context, key string) (PendingUpload, error) {
	p := PendingUpload{Key: key}
	err := s.db.QueryRowContext(ctx, `SELECT session, file FROM uploads WHERE request = ? AND state = ?`, key, uploadPending).Scan(&p.Session, &p.File)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingUpload{}, ErrNotFound
	}
	return p, err
}

func scanUpload(scan func(...any) error, state *string) (UploadRow, error) {
	var row UploadRow
	var created int64
	err := scan(&row.ID, &row.Session, &row.File, &row.Name, &row.Mime, &row.Size, &created, state)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadRow{}, ErrNotFound
	}
	row.CreatedAt = fromMillis(created)
	return row, err
}

// Upload is stored upload id.
func (s *Store) Upload(ctx context.Context, id int64) (UploadRow, error) {
	var state string
	row, err := scanUpload(s.db.QueryRowContext(ctx, `SELECT id, session, file, name, mime, size, created_at, state FROM uploads WHERE id = ?`, id).Scan, &state)
	if err == nil && state != uploadStored {
		return UploadRow{}, ErrNotFound
	}
	return row, err
}

// ErrNotSessions is an upload id that names no upload of the session.
var ErrNotSessions = errors.New("no upload of the session")

// SessionUploads are session's stored uploads ids, in their order, an id
// named twice once. An id that is no stored upload of session is
// ErrNotSessions, with a message that names it.
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
