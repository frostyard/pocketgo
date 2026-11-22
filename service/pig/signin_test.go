package pig

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/signins"
)

// signInEngine is an engine on the sign-ins at path, as Open makes it,
// with the store's clock and Copilot's exchange faked: exchange gets the
// GitHub token, and no request leaves the test.
func signInEngine(t *testing.T, path string, exchange func(token string) (ai.Credential, error)) *Engine {
	t.Helper()
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	before, beforeNow := copilotExchange, credentialNow
	t.Cleanup(func() { copilotExchange, credentialNow = before, beforeNow })
	credentialNow = func() time.Time { return now }
	copilotExchange = func(_ context.Context, p *ai.ModelsProvider, token string) (ai.Credential, error) {
		if p == nil || p.ID != signins.Copilot {
			t.Errorf("the exchange went to %+v, want PiG's GitHub Copilot", p)
		}
		return exchange(token)
	}
	models, auth, _, err := signedIn(path, profiles.Own{})
	if err != nil {
		t.Fatal(err)
	}
	return &Engine{models: models, auth: auth}
}

// copilotCredential is Copilot's sign-in for GitHub token token, as PiG's
// exchange makes it, offering the models ids.
func copilotCredential(t *testing.T, token string, ids ...string) ai.Credential {
	t.Helper()
	listed, err := json.Marshal(ids)
	if err != nil {
		t.Fatal(err)
	}
	cred := ai.Credential{Type: ai.CredentialOAuth, Refresh: token, Access: "tid=copilot-access-secret", AvailableModelIDs: listed}
	cred.SetExpiresMillis(float64(time.Date(2026, 11, 22, 12, 30, 0, 0, time.UTC).UnixMilli()))
	return cred
}

// copilotModels are the ids of PiG's GitHub Copilot catalog.
func copilotModels(t *testing.T, e *Engine) []string {
	t.Helper()
	var ids []string
	for _, m := range e.models.GetModels(signins.Copilot) {
		ids = append(ids, m.ID)
	}
	if len(ids) < 2 {
		t.Fatalf("PiG's catalog has %d Copilot models", len(ids))
	}
	return ids
}

func listedOf(t *testing.T, e *Engine, provider string) []string {
	t.Helper()
	list, err := e.Models(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range list {
		if m.Provider == provider {
			ids = append(ids, m.ID)
		}
	}
	return ids
}

// A device flow's sign-in is exchanged as PiG exchanges it and written
// through the engine's own credential store, the one its models read and
// PiG refreshes through: the Copilot models the account offers list at
// once, with no restart, the other sign-ins stay, and a sign-out takes the
// sign-in out of the same store, so the models go (ADR-0062).
func TestASignInGoesThroughTheEnginesOwnCredentialStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"deepseek": {"type": "api_key", "key": "sk-test"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var offered []string
	e := signInEngine(t, path, func(token string) (ai.Credential, error) {
		if token != "gho_device_token" {
			t.Errorf("the exchange got %q", token)
		}
		return copilotCredential(t, token, offered...), nil
	})
	offered = copilotModels(t, e)[:1]
	ctx := context.Background()
	if ids := listedOf(t, e, signins.Copilot); len(ids) != 0 {
		t.Fatalf("Copilot models before a sign-in: %v", ids)
	}
	if stored, err := e.StoredSignIn(ctx, signins.Copilot); err != nil || signins.State(stored) != proto.SignInNone {
		t.Fatalf("stored before a sign-in %+v %v", stored, err)
	}
	identity, err := e.KeepSignIn(ctx, signins.Copilot, "gho_device_token")
	if err != nil || identity == "" {
		t.Fatalf("KeepSignIn %q %v", identity, err)
	}
	if ids := listedOf(t, e, signins.Copilot); !slices.Equal(ids, offered) {
		t.Fatalf("Copilot models after the sign-in %v, want %v", ids, offered)
	}
	if ids := listedOf(t, e, "deepseek"); len(ids) == 0 {
		t.Fatal("the other sign-in's models went")
	}
	stored, err := e.StoredSignIn(ctx, signins.Copilot)
	if err != nil || signins.State(stored) != proto.SignInSignedIn || stored.Identity != identity || !stored.Refreshable || stored.Expired {
		t.Fatalf("stored after the sign-in %+v %v", stored, err)
	}
	if strings.Contains(identity, "gho_device_token") {
		t.Fatal("the identity holds the token")
	}
	var file map[string]map[string]any
	data, err := os.ReadFile(path)
	if err != nil || json.Unmarshal(data, &file) != nil {
		t.Fatalf("auth.json %s %v", data, err)
	}
	if file[signins.Copilot]["refresh"] != "gho_device_token" || file["deepseek"]["key"] != "sk-test" {
		t.Fatalf("auth.json holds %s", data)
	}

	if err := e.SignOut(ctx, signins.Copilot); err != nil {
		t.Fatal(err)
	}
	if ids := listedOf(t, e, signins.Copilot); len(ids) != 0 {
		t.Fatalf("Copilot models after the sign-out: %v", ids)
	}
	if ids := listedOf(t, e, "deepseek"); len(ids) == 0 {
		t.Fatal("the sign-out took the other sign-in")
	}
	data, _ = os.ReadFile(path)
	if strings.Contains(string(data), signins.Copilot) || !strings.Contains(string(data), "sk-test") {
		t.Fatalf("auth.json after the sign-out: %s", data)
	}
	if err := e.SignOut(ctx, signins.Copilot); err != nil {
		t.Fatalf("a second sign-out: %v", err)
	}
}

// A sign-in waits for the store's lock, which PiG's refresh holds while it
// refreshes, and is written after it: one writer, in turn (ADR-0005,
// ADR-0062).
func TestASignInWaitsForARefreshHoldingTheStore(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	exchanged := make(chan struct{})
	var e *Engine
	e = signInEngine(t, path, func(token string) (ai.Credential, error) {
		close(exchanged)
		return copilotCredential(t, token, copilotModels(t, e)[0]), nil
	})
	ctx := context.Background()
	held, release := make(chan struct{}), make(chan struct{})
	refreshed := make(chan error, 1)
	go func() {
		// PiG's refresh: under the store's lock, it finds nothing to do.
		_, err := e.auth.Modify(ctx, signins.Copilot, func(*ai.Credential) (*ai.Credential, error) {
			close(held)
			<-release
			return nil, nil
		})
		refreshed <- err
	}()
	<-held
	kept := make(chan error, 1)
	go func() { _, err := e.KeepSignIn(ctx, signins.Copilot, "gho_token"); kept <- err }()
	<-exchanged
	select {
	case err := <-kept:
		t.Fatalf("the sign-in was kept while a refresh held the store: %v", err)
	default:
	}
	close(release)
	if err := <-refreshed; err != nil {
		t.Fatal(err)
	}
	if err := <-kept; err != nil {
		t.Fatal(err)
	}
	if stored, _ := e.StoredSignIn(ctx, signins.Copilot); signins.State(stored) != proto.SignInSignedIn {
		t.Fatalf("stored %+v", stored)
	}
}

// Only a device flow's provider signs in and out from a client; an
// exchange Copilot refused writes nothing, and its error carries no
// token; a sign-in that is no subscription's is refused.
func TestKeepSignInRefusesWhatItDoesNotKeep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	answer := func(string) (ai.Credential, error) { return ai.Credential{}, errors.New("401 Unauthorized") }
	e := signInEngine(t, path, func(token string) (ai.Credential, error) { return answer(token) })
	ctx := context.Background()
	if _, err := e.KeepSignIn(ctx, "anthropic", "t"); !errors.Is(err, signins.ErrNoDeviceFlow) {
		t.Fatalf("anthropic's sign-in kept: %v", err)
	}
	if err := e.SignOut(ctx, "anthropic"); !errors.Is(err, signins.ErrNoDeviceFlow) {
		t.Fatalf("anthropic signed out: %v", err)
	}
	if _, err := e.KeepSignIn(ctx, signins.Copilot, "gho_secret"); err == nil || strings.Contains(err.Error(), "gho_secret") {
		t.Fatalf("a refused exchange: %v", err)
	}
	answer = func(token string) (ai.Credential, error) {
		return ai.Credential{Type: ai.CredentialAPIKey, Key: token}, nil
	}
	if _, err := e.KeepSignIn(ctx, signins.Copilot, "gho_secret"); err == nil {
		t.Fatal("a key was kept as Copilot's sign-in")
	}
	if stored, _ := e.StoredSignIn(ctx, signins.Copilot); stored.Kind != "" {
		t.Fatalf("a refused sign-in stored %+v", stored)
	}
	if _, err := (&Engine{}).KeepSignIn(ctx, signins.Copilot, "t"); !errors.Is(err, errNoStore) {
		t.Fatalf("an engine with no store kept a sign-in: %v", err)
	}
}

// What the state is read from: an expired access token with a refresh
// token is still signed in, as PiG refreshes it; one with none is
// expired.
func TestStoredOfReadsTheExpiryAndTheRefreshToken(t *testing.T) {
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	cred := ai.Credential{Type: ai.CredentialOAuth, Access: "a"}
	cred.SetExpiresMillis(float64(now.Add(-time.Minute).UnixMilli()))
	if s := storedOf(cred, now); signins.State(s) != proto.SignInExpired || s.Identity == "" {
		t.Fatalf("an expired sign-in with no refresh token: %+v", s)
	}
	cred.Refresh = "r"
	if s := storedOf(cred, now); signins.State(s) != proto.SignInSignedIn {
		t.Fatalf("an expired sign-in with a refresh token: %+v", s)
	}
	if s := storedOf(ai.Credential{Type: ai.CredentialAPIKey, Key: "k"}, now); signins.State(s) != proto.SignInNone || s.Identity != "" {
		t.Fatalf("a key: %+v", s)
	}
}
