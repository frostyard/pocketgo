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
// type and Size its bytes, as uploaded; Digest is its bytes' SHA-256 in
// hexadecimal, kept when it was staged, "" before.
type UploadRow struct {
	ID        int64
	Session   int64
	File      string
	Name      string
	Mime      string
	Size      int64
	Digest    string
	CreatedAt time.Time
}

// An upload is claimed, pending, before its file is written; staged once
// its file is written whole, with what tells that file apart (its device,
// inode and size) and before it is named; and stored once it is named. A
// stop of pocketd before the name leaves nothing in the uploads folder; one
// after it leaves a staged upload, which the next try or start finishes
// while its name holds the file it staged, and drops otherwise: pocketd
// removes nothing there (Codex review of #49, rounds 1 to 3). Only a stored
// upload is an upload.
const (
	uploadPending = "pending"
	uploadStored  = "stored"
)

// ClaimUpload claims key, of kind, for row, session's new upload of type
// row.Mime, pending, by pocketd's run run: the request's claim and the
// pending row go in one transaction. A key recorded before for kind
// answers its upload, with existed set: stored when its request has a
// result, else the pending one a try left. A key recorded for another kind
// is ErrRequestReused.
func (s *Store) ClaimUpload(ctx context.Context, key, kind string, row UploadRow, run string) (out UploadRow, pending, existed bool, err error) {
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
		out, err = scanUpload(tx.QueryRowContext(ctx, uploadColumns+` WHERE request = ?`, key).Scan, &state)
		return out, state == uploadPending, true, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO uploads (session, file, name, mime, size, request, created_at, state, run) VALUES (?, ?, ?, ?, 0, ?, ?, ?, ?)`,
		row.Session, row.File, row.Name, row.Mime, key, now, uploadPending, run)
	if err != nil {
		return UploadRow{}, false, false, err
	}
	if row.ID, err = res.LastInsertId(); err != nil {
		return UploadRow{}, false, false, err
	}
	row.CreatedAt, row.Size = fromMillis(now), 0
	return row, true, false, tx.Commit()
}

// StageUpload keeps, with key's pending upload of file, what tells the
// file pocketd wrote apart, before it names it: its device, its inode, its
// size and its bytes' digest.
func (s *Store) StageUpload(ctx context.Context, key, file string, dev, ino uint64, size int64, digest string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE uploads SET dev = ?, ino = ?, size = ?, digest = ?, staged = 1 WHERE request = ? AND file = ? AND state = ?`,
		int64(dev), int64(ino), size, digest, key, file, uploadPending) //nolint:gosec // an inode's bits, kept as they are
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n != 1 {
		return fmt.Errorf("no pending upload of %s: %w", file, ErrNotFound)
	}
	return nil
}

// FinishUpload stores key's staged upload and completes its request, in
// one transaction: the file is named.
func (s *Store) FinishUpload(ctx context.Context, key string) (UploadRow, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return UploadRow{}, err
	}
	defer func() { _ = tx.Rollback() }()
	var state string
	row, err := scanUpload(tx.QueryRowContext(ctx, uploadColumns+` WHERE request = ?`, key).Scan, &state)
	if err != nil {
		return UploadRow{}, err
	}
	var staged bool
	if err := tx.QueryRowContext(ctx, `SELECT staged FROM uploads WHERE id = ?`, row.ID).Scan(&staged); err != nil {
		return UploadRow{}, err
	}
	if state != uploadPending || !staged {
		return UploadRow{}, fmt.Errorf("upload %d is %s, not staged: %w", row.ID, state, ErrConflict)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE uploads SET state = ? WHERE id = ?`, uploadStored, row.ID); err != nil {
		return UploadRow{}, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE requests SET result = ? WHERE key = ?`, strconv.FormatInt(row.ID, 10), key); err != nil {
		return UploadRow{}, err
	}
	return row, tx.Commit()
}

// DropUpload removes key's pending upload of file and frees its claim, in
// one transaction: a request that failed leaves no record. An upload of
// another file under key, as a retry claims one, a stored upload, and
// their request, stay (Codex review of #49, round 2).
func (s *Store) DropUpload(ctx context.Context, key, file string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.ExecContext(ctx, `DELETE FROM uploads WHERE request = ? AND file = ? AND state = ?`, key, file, uploadPending); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM requests WHERE key = ? AND result = '' AND NOT EXISTS (SELECT 1 FROM uploads WHERE request = ?)`, key, key); err != nil {
		return err
	}
	return tx.Commit()
}

// PendingUpload is an upload claimed and not stored: its request's key,
// its file, and, once it is staged, what tells its file apart.
type PendingUpload struct {
	Key      string
	Session  int64
	File     string
	Staged   bool
	Dev, Ino uint64
	Size     int64
	Digest   string
}

const pendingColumns = `SELECT request, session, file, staged, dev, ino, size, digest FROM uploads`

func scanPending(scan func(...any) error) (PendingUpload, error) {
	var p PendingUpload
	var dev, ino int64
	err := scan(&p.Key, &p.Session, &p.File, &p.Staged, &dev, &ino, &p.Size, &p.Digest)
	if errors.Is(err, sql.ErrNoRows) {
		return PendingUpload{}, ErrNotFound
	}
	p.Dev, p.Ino = uint64(dev), uint64(ino) //nolint:gosec // an inode's bits, kept as they are
	return p, err
}

// PendingUploads are the uploads another run of pocketd than run claimed
// and never stored: a stop left them, and no request of this run writes
// them, whatever the clock did (Codex review of #49, round 3).
func (s *Store) PendingUploads(ctx context.Context, run string) ([]PendingUpload, error) {
	rows, err := s.db.QueryContext(ctx, pendingColumns+` WHERE state = ? AND run != ? ORDER BY id`, uploadPending, run)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []PendingUpload
	for rows.Next() {
		p, err := scanPending(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PendingUploadOf is key's pending upload, if it has one.
func (s *Store) PendingUploadOf(ctx context.Context, key string) (PendingUpload, error) {
	return scanPending(s.db.QueryRowContext(ctx, pendingColumns+` WHERE request = ? AND state = ?`, key, uploadPending).Scan)
}

const uploadColumns = `SELECT id, session, file, name, mime, size, digest, created_at, state FROM uploads`

func scanUpload(scan func(...any) error, state *string) (UploadRow, error) {
	var row UploadRow
	var created int64
	err := scan(&row.ID, &row.Session, &row.File, &row.Name, &row.Mime, &row.Size, &row.Digest, &created, state)
	if errors.Is(err, sql.ErrNoRows) {
		return UploadRow{}, ErrNotFound
	}
	row.CreatedAt = fromMillis(created)
	return row, err
}

// Upload is stored upload id.
func (s *Store) Upload(ctx context.Context, id int64) (UploadRow, error) {
	var state string
	row, err := scanUpload(s.db.QueryRowContext(ctx, uploadColumns+` WHERE id = ?`, id).Scan, &state)
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
