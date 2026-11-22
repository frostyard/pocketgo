package pig

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/mcp/oauth"
)

// The guards of the Codex review of #126, round 11.

// loginWithNotesHeader runs pocketd mcp login for the gateway with the
// notes server's header kept, refusing before it prints a page, and
// returns its error and what it printed.
func loginWithNotesHeader(t *testing.T) (string, error) {
	t.Helper()
	dir := t.TempDir()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", notesKey); err != nil {
		t.Fatal(err)
	}
	server, _ := mcpServers(t).Find("gateway")
	var out bytes.Buffer
	err = MCPLogin(context.Background(), dir, server, strings.NewReader(""), &out)
	if err == nil || strings.Contains(out.String(), "Open this page") {
		t.Fatalf("the sign-in went on (%v): %s", err, out.String())
	}
	return out.String(), err
}

// pocketd sends no request whose address holds a secret it keeps, as a
// challenge's metadata address may when the server echoes a header.
func TestADiscoveredAddressHoldingASecretIsNotFetched(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.metadataQuery = "k=" + url.QueryEscape("notes-secret-value") })
	loginWithNotesHeader(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, seen := range fake.seen {
		if strings.Contains(seen, "secret") {
			t.Fatalf("pocketd sent %s", seen)
		}
	}
}

// pocketd mcp login prints no authorization page that holds a secret it
// keeps.
func TestMCPLoginPrintsNoPageHoldingASecret(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.pageQuery = "tenant=" + url.QueryEscape("notes-secret-value") })
	printed, err := loginWithNotesHeader(t)
	if strings.Contains(printed, "secret") || strings.Contains(err.Error(), "notes-secret-value") {
		t.Fatalf("printed %s (%v)", printed, err)
	}
}

// A refusal for more scope never names a secret, even one with a space
// whose every word passes alone.
func TestAStepUpScopeHoldsNoSpacedSecret(t *testing.T) {
	secrets := []string{"alpha-one beta-two"}
	for _, scope := range []string{"alpha-one beta-two", "x alpha-one   beta-two"} {
		if got := challengeScope(scope, secrets); got != "" {
			t.Errorf("%q: %q", scope, got)
		}
	}
	if got := challengeScope("alpha-one", secrets); got != "alpha-one" {
		t.Errorf("a word alone: %q", got)
	}
}

// A credentials write that failed is made a last time at pocketd's stop,
// while it still holds the data folder's lock: a rotated refresh token
// the disk could not take reaches it once it mends.
func TestTheStopWritesTheCredentialsAFailedWriteLeft(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	auth := filepath.Join(f.dataDir, MCPAuthFile)
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := (mcpOAuthStore{store: e.mcp.store, name: "gateway", url: gatewayURL}).Save(context.Background(),
		oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: "a-2", RefreshToken: "rotated-2"}}); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	e.mcp.close(context.Background())
	again, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if state := again.oauthState("gateway", gatewayURL); state == nil || state.Tokens == nil || state.Tokens.RefreshToken != "rotated-2" {
		t.Fatalf("kept %+v", state)
	}
}

// A Bearer challenge is read alone: a later challenge's scope is not its
// own.
func TestABearerChallengeIsReadAlone(t *testing.T) {
	header := http.Header{}
	header.Add("WWW-Authenticate", `Bearer resource_metadata="https://mcp.example.com/m", Other scope="theirs"`)
	if got := oauthChallenge(header); got.Scope != "" || got.ResourceMetadataURL == nil {
		t.Fatalf("challenge %+v", got)
	}
	header = http.Header{}
	header.Add("WWW-Authenticate", `Basic realm="r"`)
	header.Add("WWW-Authenticate", `Bearer scope="mine", error="insufficient_scope"`)
	if got := oauthChallenge(header); got.Scope != "mine" || got.Error != "insufficient_scope" {
		t.Fatalf("challenge %+v", got)
	}
}

// A listing whose entry is null, or a tool with no name, is no list: the
// last one stays.
func TestAListingOfANullEntryIsNoList(t *testing.T) {
	for _, page := range []string{`{"tools":[null]}`, `{"tools":[{"description":"no name"}]}`, `{"tools":["search"]}`} {
		if _, _, err := decodeToolsPage(json.RawMessage(page), 8); err == nil {
			t.Errorf("%s is a list", page)
		}
	}
}

// pocketd mcp login stops before the owner approves anything when the
// token endpoint is one PiG's exchange refuses: plain HTTP on a host that
// is not loopback, a tailnet name included.
func TestMCPLoginNeedsATokenEndpointPiGTakes(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.tokenEndpoint = "http://auth.example.ts.net/token" })
	server, _ := mcpServers(t).Find("gateway")
	var out bytes.Buffer
	err := MCPLogin(context.Background(), t.TempDir(), server, strings.NewReader(""), &out)
	if err == nil || !strings.Contains(err.Error(), "token endpoint") || strings.Contains(out.String(), "Open this page") {
		t.Fatalf("the sign-in went on (%v): %s", err, out.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 0 {
		t.Fatalf("registered %d", fake.registered)
	}
}
