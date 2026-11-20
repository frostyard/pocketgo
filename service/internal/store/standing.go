package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
)

// A profile's standing session (ADR-0026) is the one session New Session
// opens for that profile, for good: one a profile, and a session is the
// standing session of one profile at most.

// CreateStandingSession records row, a new session, as the standing
// session of its profile, with the request key that created it, in one
// transaction (CreateSession). A profile whose standing session is another
// one already is ErrConflict, and nothing is recorded.
func (s *Store) CreateStandingSession(ctx context.Context, key string, row SessionRow) (SessionRow, error) {
	if row.Profile == "" {
		return SessionRow{}, errors.New("a standing session is a profile's")
	}
	row.Standing = true
	return s.createSession(ctx, key, row, func(tx *sql.Tx, now int64) error {
		if _, err := tx.ExecContext(ctx, `INSERT INTO standing_sessions (profile, session, created_at) VALUES (?, ?, ?)
			ON CONFLICT (profile) DO NOTHING`, row.Profile, row.ID, now); err != nil {
			return err
		}
		var session int64
		if err := tx.QueryRowContext(ctx, `SELECT session FROM standing_sessions WHERE profile = ?`, row.Profile).Scan(&session); err != nil {
			return err
		}
		if session != row.ID {
			return fmt.Errorf("%w: the standing session of %s is session %d", ErrConflict, row.Profile, session)
		}
		return nil
	})
}

// StandingSession is the standing session of profile, ErrNotFound for
// none.
func (s *Store) StandingSession(ctx context.Context, profile string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT session FROM standing_sessions WHERE profile = ?`, profile).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	return id, err
}

// StandingSessions are every profile's standing session, by id.
func (s *Store) StandingSessions(ctx context.Context) ([]int64, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT session FROM standing_sessions ORDER BY session`)
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

// StandingProfile is the profile whose standing session session is,
// ErrNotFound when it is no standing session.
func (s *Store) StandingProfile(ctx context.Context, session int64) (string, error) {
	var profile string
	err := s.db.QueryRowContext(ctx, `SELECT profile FROM standing_sessions WHERE session = ?`, session).Scan(&profile)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return profile, err
}

// SetSessionModel records the model a session runs now, and the router's
// rule that chose it, as a standing session takes its profile's new model
// (ADR-0026). The thinking level goes back to the model's default.
func (s *Store) SetSessionModel(ctx context.Context, id int64, provider, model, route string) error {
	result, err := s.db.ExecContext(ctx, `UPDATE sessions SET provider = ?, model = ?, route = ?, thinking = '' WHERE id = ?`, provider, model, route, id)
	if err != nil {
		return err
	}
	if n, _ := result.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}
