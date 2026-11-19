package config

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Keyring is the part of a credential store the client uses: mygo.Secrets
// in the app, a map in tests.
type Keyring interface {
	Set(key string, value []byte) error
	Get(key string) ([]byte, error)
	Delete(key string) error
}

// Credentials keeps the device token of each pocketd: in the keyring, and
// in a file of the config folder that only the user can read only where
// there is no keyring. A keyring is missing when its methods fail with an
// error for which errors.Is(err, errors.ErrUnsupported) holds, as
// mygo.Secrets does on a Linux desktop without a Secret Service; a nil Ring
// is the same. Any other keyring error is returned, never a reason to use
// the file, and a working keyring without the token means there is none,
// whatever a file left from a time without a keyring holds.
type Credentials struct {
	Ring Keyring
	// NotFound is what Ring.Get fails with for a missing key.
	NotFound error
	Dir      string
}

func credKey(server string) string { return "token:" + strings.TrimRight(server, "/") }

func (c Credentials) file(server string) string {
	name := strings.NewReplacer("://", "_", "/", "_", ":", "_", "[", "_", "]", "_").Replace(strings.TrimRight(server, "/"))
	return filepath.Join(c.Dir, "token-"+name)
}

// unsupported reports whether err says there is no keyring.
func unsupported(err error) bool { return errors.Is(err, errors.ErrUnsupported) }

// Token returns the token kept for server, or "" when there is none.
func (c Credentials) Token(server string) (string, error) {
	if c.Ring != nil {
		b, err := c.Ring.Get(credKey(server))
		switch {
		case err == nil:
			return string(b), nil
		case c.NotFound != nil && errors.Is(err, c.NotFound):
			return "", nil
		case !unsupported(err):
			return "", err
		}
	}
	b, err := os.ReadFile(c.file(server))
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}
	return strings.TrimSpace(string(b)), err
}

// SetToken keeps token for server, replacing what was kept. With a working
// keyring, a token file left from a time without one is removed.
func (c Credentials) SetToken(server, token string) error {
	if c.Ring != nil {
		err := c.Ring.Set(credKey(server), []byte(token))
		if err == nil {
			_ = os.Remove(c.file(server))
			return nil
		}
		if !unsupported(err) {
			return err
		}
	}
	return os.WriteFile(c.file(server), []byte(token), 0o600)
}

// Forget removes the token of server, from the keyring and the file. It
// returns what failed, but a token that is not there: a locked keyring
// that keeps the token is an error the owner must see.
func (c Credentials) Forget(server string) error {
	var errs []error
	if c.Ring != nil {
		err := c.Ring.Delete(credKey(server))
		if err != nil && !unsupported(err) && (c.NotFound == nil || !errors.Is(err, c.NotFound)) {
			errs = append(errs, fmt.Errorf("removing the token from the keyring: %w", err))
		}
	}
	if err := os.Remove(c.file(server)); err != nil && !errors.Is(err, os.ErrNotExist) {
		errs = append(errs, fmt.Errorf("removing the token file: %w", err))
	}
	return errors.Join(errs...)
}
