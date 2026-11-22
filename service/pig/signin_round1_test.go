package pig

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/service/internal/signins"
)

// A failed exchange says so in KeepSignIn's own words: PiG's error, which
// carries the answer's body, could echo the GitHub token or Copilot's, and
// a flow's error goes to every client (Codex review of #123, round 1).
func TestAFailedExchangeNamesNoSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	e := signInEngine(t, path, func(token string) (ai.Credential, error) {
		return ai.Credential{}, errors.New(`list models: 500 Internal Server Error: {"echo":"Bearer tid=copilot-secret ` + token + `"}`)
	})
	_, err := e.KeepSignIn(context.Background(), signins.Copilot, "gho_device_secret")
	if err == nil {
		t.Fatal("a failed exchange kept a sign-in")
	}
	for _, secret := range []string{"tid=copilot-secret", "gho_device_secret", "Internal Server Error"} {
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("the error %q carries PiG's text %q", err, secret)
		}
	}
	if !errors.Is(err, errExchange) {
		t.Fatalf("the error %v is not the exchange's", err)
	}
}
