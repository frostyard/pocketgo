package store

import (
	"context"
	"database/sql"
	"errors"
)

// SignInAccount is the account kept for provider's sign-in (ADR-0062):
// the login its device flow named, with the identity of the credential it
// was kept with; false for none.
func (s *Store) SignInAccount(ctx context.Context, provider string) (identity, account string, ok bool, err error) {
	err = s.db.QueryRowContext(ctx, `SELECT identity, account FROM signin_accounts WHERE provider = ?`, provider).Scan(&identity, &account)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		return "", "", false, nil
	case err != nil:
		return "", "", false, err
	}
	return identity, account, true, nil
}

// KeepSignInAccount keeps account for provider's sign-in, the credential
// identity names, in place of any kept before.
func (s *Store) KeepSignInAccount(ctx context.Context, provider, identity, account string) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO signin_accounts (provider, identity, account, kept_at) VALUES (?, ?, ?, ?)
		ON CONFLICT (provider) DO UPDATE SET identity = excluded.identity, account = excluded.account, kept_at = excluded.kept_at`,
		provider, identity, account, s.millis())
	return err
}

// ForgetSignInAccount forgets the account kept for provider's sign-in, as
// a sign-out does.
func (s *Store) ForgetSignInAccount(ctx context.Context, provider string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM signin_accounts WHERE provider = ?`, provider)
	return err
}
