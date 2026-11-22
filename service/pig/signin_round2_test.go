package pig

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/service/internal/signins"
)

// A sign-ins file that does not parse is said to be unreadable in
// pocketd's words: PiG's parse error may quote the file, a credential
// included, and the error is logged (Codex review of #123, round 2).
func TestAnUnreadableSignInsFileNamesNoSecret(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	e := signInEngine(t, path, func(string) (ai.Credential, error) { return ai.Credential{}, nil })
	if err := os.WriteFile(path, []byte(`{"github-copilot": {"type": "oauth", "refresh": "gho_file_secret" "access": "x"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	_, readErr := e.StoredSignIn(ctx, signins.Copilot)
	outErr := e.SignOut(ctx, signins.Copilot)
	for _, err := range []error{readErr, outErr} {
		if err == nil || strings.Contains(err.Error(), "gho_file_secret") || err != errUnreadable {
			t.Fatalf("an unreadable file: %v", err)
		}
	}
}

// A sign-out takes a subscription's sign-in alone: an API key the file
// holds for the provider, which pocketd lists as no sign-in, stays (Codex
// review of #123, round 2).
func TestASignOutLeavesAKeyThatIsNoSignIn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, []byte(`{"github-copilot": {"type": "api_key", "key": "ghu_owners_key"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	e := signInEngine(t, path, func(string) (ai.Credential, error) { return ai.Credential{}, nil })
	if err := e.SignOut(context.Background(), signins.Copilot); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || !strings.Contains(string(data), "ghu_owners_key") {
		t.Fatalf("the sign-out took the key: %s %v", data, err)
	}
}
