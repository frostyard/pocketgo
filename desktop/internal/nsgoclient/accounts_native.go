//go:build !js

package nsgoclient

import (
	"errors"
	"os"

	"github.com/frostyard/pocketgo/desktop/internal/config"
	"github.com/frostyard/pocketgo/desktop/internal/ownerauth"
	"github.com/nsgo-dev/nsgo/core"
)

type keyring struct{ secrets core.Secrets }

func secretError(err error) error {
	if errors.Is(err, core.ErrSecretsUnavailable) {
		return errors.ErrUnsupported
	}
	return err
}
func (k keyring) Get(key string) ([]byte, error) {
	v, ok, err := k.secrets.Get(key)
	if err != nil {
		return nil, secretError(err)
	}
	if !ok {
		return nil, os.ErrNotExist
	}
	return []byte(v), nil
}
func (k keyring) Set(key string, b []byte) error { return secretError(k.secrets.Set(key, string(b))) }
func (k keyring) Delete(key string) error        { return secretError(k.secrets.Delete(key)) }
func (c *client) platformAccounts() {
	store, err := config.Open(os.Getenv("POCKETGO_CONFIG_DIR"))
	if err != nil {
		c.result(err)
		return
	}
	// A device that ran the MyGo client starts with what it kept. A file
	// that cannot be read moves nothing.
	if settings, err := store.Load(); err == nil {
		c.moveSettings(settings)
	}
	secret := c.cx.Secrets()
	// Both desktop clients share the same owner sign-in lock and keyring identity.
	secret.Service = config.AppID
	ring := keyring{secret}
	c.creds = config.Credentials{Ring: ring, NotFound: os.ErrNotExist, Dir: store.Dir()}
	auth := ownerauth.New(config.GitHubApp{Ring: ring, NotFound: os.ErrNotExist}, "")
	c.initGitHub(auth)
	c.cx.Cleanup(auth.Close)
}
