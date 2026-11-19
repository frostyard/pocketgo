package config

import (
	"errors"
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

// Credentials keeps the device token of each pocketd: in the keyring when
// there is one, else in a file of the config folder that only the user can
// read. A keyring is missing when its methods fail with an error for which
// errors.Is(err, errors.ErrUnsupported) holds, as mygo.Secrets does on a
// Linux desktop without a Secret Service; any other keyring error is
// returned, never a reason to write the token to a file.
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

// Token returns the token kept for server, or "" when there is none.
func (c Credentials) Token(server string) (string, error) {
	if c.Ring != nil {
		b, err := c.Ring.Get(credKey(server))
		switch {
		case err == nil:
			return string(b), nil
		case c.NotFound != nil && errors.Is(err, c.NotFound):
		case errors.Is(err, errors.ErrUnsupported):
		default:
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
		if !errors.Is(err, errors.ErrUnsupported) {
			return err
		}
	}
	return os.WriteFile(c.file(server), []byte(token), 0o600)
}

// Forget removes the token of server, from the keyring and the file.
func (c Credentials) Forget(server string) error {
	if c.Ring != nil {
		_ = c.Ring.Delete(credKey(server))
	}
	err := os.Remove(c.file(server))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
