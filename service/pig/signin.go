package pig

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/service/internal/signins"
)

// The sign-ins a client makes (ADR-0062) go through the engine's own
// credential store: e.auth, the AuthStorage its models' keptCredentials
// wrap, whose Modify PiG refreshes a subscription's tokens through under
// the store's lock. pocketd opens no second store of auth.json while it
// serves, so a sign-in, a sign-out and a refresh never write past one
// another: each reads the file under that lock and writes it whole.

// errNoStore is an engine opened without the sign-ins (tests).
var errNoStore = errors.New("the engine has no credential store")

// credentialNow is the clock a stored sign-in's expiry is read on; tests
// replace it.
var credentialNow = time.Now

// copilotExchange exchanges a GitHub token of Copilot's OAuth app for
// Copilot's sign-in as PiG's own sign-in and refresh do: the provider's
// OAuth refresh, which gets Copilot's token and the account's models.
// Tests replace it, so none reaches GitHub.
var copilotExchange = func(ctx context.Context, p *ai.ModelsProvider, githubToken string) (ai.Credential, error) {
	if p == nil || p.Auth.OAuth == nil || p.Auth.OAuth.Refresh == nil {
		return ai.Credential{}, errors.New("PiG signs in to no GitHub Copilot")
	}
	return p.Auth.OAuth.Refresh(ctx, ai.Credential{Type: ai.CredentialOAuth, Refresh: githubToken})
}

// StoredSignIn is what the engine's credential store holds for provider,
// with no secret in it (signins.State decides its state from it).
func (e *Engine) StoredSignIn(_ context.Context, provider string) (signins.Stored, error) {
	if e.auth == nil {
		return signins.Stored{}, errNoStore
	}
	cred, ok, err := e.auth.GetRaw(provider)
	if err != nil || !ok {
		return signins.Stored{}, err
	}
	return storedOf(cred, credentialNow()), nil
}

// storedOf is what State and Account read of cred, no secret in it.
func storedOf(cred ai.Credential, now time.Time) signins.Stored {
	s := signins.Stored{Kind: string(cred.Type)}
	if cred.Type != ai.CredentialOAuth {
		return s
	}
	s.Refreshable = cred.Refresh != ""
	if cred.HasExpires() {
		s.Expired = float64(now.UnixMilli()) >= cred.ExpiresMillis()
	}
	s.Identity = signInIdentity(cred)
	return s
}

// signInIdentity names a subscription's sign-in by a digest of its refresh
// token, or of its access token when it has none: no secret, and the same
// for as long as the credential is the same one (Copilot's refresh keeps
// its GitHub token).
func signInIdentity(cred ai.Credential) string {
	secret := cred.Refresh
	if secret == "" {
		secret = cred.Access
	}
	if secret == "" {
		return ""
	}
	sum := sha256.Sum256([]byte("pocketgo/signin\x00" + secret))
	return hex.EncodeToString(sum[:16])
}

// KeepSignIn keeps provider's sign-in from token, the GitHub token of an
// approved device flow: exchanged for Copilot's as PiG does, then written
// through the engine's own credential store, in place of the sign-in it
// held, so the models list it at once (Models reads the store at each
// call). It says which credential it kept (signins.Stored.Identity). Only
// Copilot signs in so (signins.Providers).
//
// Its errors are its own words, never PiG's: PiG's exchange and its model
// list put the answer's body in their errors, which could echo the GitHub
// token or Copilot's, and a flow's error goes to every client (Codex
// review of #123, round 1).
func (e *Engine) KeepSignIn(ctx context.Context, provider, token string) (string, error) {
	if p, ok := signins.Find(provider); !ok || !p.Device || provider != signins.Copilot {
		return "", signins.ErrNoDeviceFlow
	}
	if e.auth == nil {
		return "", errNoStore
	}
	cred, err := copilotExchange(ctx, e.models.GetProvider(provider), token)
	switch {
	case ctx.Err() != nil:
		return "", errKeepStopped
	case err != nil:
		return "", errExchange
	case cred.Type != ai.CredentialOAuth || cred.Refresh == "" || cred.Access == "":
		return "", errNoExchange
	}
	if _, err := e.auth.Modify(ctx, provider, func(*ai.Credential) (*ai.Credential, error) { return &cred, nil }); err != nil {
		if ctx.Err() != nil {
			return "", errKeepStopped
		}
		return "", errNotKept
	}
	return signInIdentity(cred), nil
}

// KeepSignIn's errors, which name no secret.
var (
	errKeepStopped = errors.New("the sign-in stopped before pocketd kept it")
	errExchange    = errors.New("the Copilot token exchange or model list refused the GitHub sign-in")
	errNoExchange  = errors.New("the Copilot exchange answered no sign-in")
	errNotKept     = errors.New("the credential store did not keep the sign-in")
)

// SignOut takes provider's sign-in out of the engine's own credential
// store, under its lock: a refresh after it finds none and writes
// nothing, and the next model call of a session on the provider fails for
// want of a sign-in. Only a provider a client signs in to signs out so.
func (e *Engine) SignOut(ctx context.Context, provider string) error {
	if p, ok := signins.Find(provider); !ok || !p.Device {
		return signins.ErrNoDeviceFlow
	}
	if e.auth == nil {
		return errNoStore
	}
	return e.auth.Delete(ctx, provider)
}
