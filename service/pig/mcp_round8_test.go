package pig

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"

	"github.com/frostyard/pocketgo/service/internal/uploads"
)

// The guards of the Codex review of #126, round 8.

// A refresh whose write failed leaves the live connection's sign-in
// usable: the call goes on with the new token, the next one too once the
// disk mends, and the store writes the token then, with no second
// exchange.
func TestARefusedWriteLeavesTheSignInUsable(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	fake.set(func() { fake.access["old-token"], fake.refresh, fake.next = true, "ref-1", "acc-2" })
	seedSignIn(t, f.dataDir, "old-token", "ref-1")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"gateway"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	c := e.mcp.conns["gateway"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	// The access token ends, and the credentials file cannot be written.
	fake.set(func() { delete(fake.access, "old-token") })
	auth := filepath.Join(f.dataDir, MCPAuthFile)
	if err := os.Remove(auth); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(auth, 0o700); err != nil {
		t.Fatal(err)
	}
	for i, mend := range []bool{false, true} {
		if mend {
			if err := os.Remove(auth); err != nil {
				t.Fatal(err)
			}
		}
		got := c.call(ctx, "echo_token", map[string]any{})
		if got.IsError != nil && *got.IsError {
			t.Fatalf("call %d: %s", i, got.Output[0].(ai.TextContent).Text)
		}
	}
	c.mu.Lock()
	after := c.client
	c.mu.Unlock()
	if before == nil || after != before {
		t.Fatal("the calls did not go through the live connection")
	}
	again, err := openMCPStore(f.dataDir)
	if err != nil {
		t.Fatal(err)
	}
	if state := again.oauthState("gateway", gatewayURL); state == nil || state.Tokens == nil || state.Tokens.AccessToken != "acc-2" {
		t.Fatalf("kept %+v", state)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	refreshes := 0
	for _, seen := range fake.seen {
		if seen == "POST "+authURL+"/token" {
			refreshes++
		}
	}
	if refreshes != 1 {
		t.Fatalf("%d token exchanges", refreshes)
	}
}

// loginRefused runs pocketd mcp login for the gateway, which must refuse
// before it prints a page, and returns its error.
func loginRefused(t *testing.T) error {
	t.Helper()
	server, _ := mcpServers(t).Find("gateway")
	var out bytes.Buffer
	err := MCPLogin(context.Background(), t.TempDir(), server, strings.NewReader(""), &out)
	if err == nil || strings.Contains(out.String(), "Open this page") {
		t.Fatalf("the sign-in went on (%v): %s", err, out.String())
	}
	return err
}

// pocketd sends nothing on plain HTTP to a host that is neither loopback
// nor a tailnet name, whatever discovery names.
func TestMCPLoginSendsNothingOnPlainHTTP(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.plainAuth = true })
	loginRefused(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, seen := range fake.seen {
		if strings.Contains(seen, "http://") {
			t.Fatalf("pocketd sent %s", seen)
		}
	}
}

// pocketd mcp login prints no authorization page on plain HTTP to such a
// host.
func TestMCPLoginRefusesAPlainHTTPPage(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.plainPage = true })
	if err := loginRefused(t); !strings.Contains(err.Error(), "plain HTTP") {
		t.Fatalf("refused with %v", err)
	}
}

// pocketd mcp login stops when its discovery fails: the scopes it asks
// for are weighed against no other.
func TestMCPLoginNeedsItsDiscovery(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.metadataFails = 1 })
	loginRefused(t)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 0 {
		t.Fatalf("registered %d", fake.registered)
	}
}

// A text block that is a JSON string alone shows no secret, whatever its
// escapes.
func TestAJSONStringShowsNoSecret(t *testing.T) {
	escape := string(rune(92)) + "u"
	text := `"notes-` + escape + `0073ecret-value"`
	if got := redactText(text, []string{"notes-secret-value"}); strings.Contains(got, "ecret-value") {
		t.Fatalf("redacted %s", got)
	}
}

// The image the model gets holds no secret: its re-encoding writes other
// bytes than the server sent.
func TestAnImageAsSentHoldsNoSecret(t *testing.T) {
	prepareImage = func([]byte) (uploads.Image, string, error) {
		return uploads.Image{Data: []byte("an image holding notes-secret-value"), MimeType: "image/png"}, "", nil
	}
	t.Cleanup(func() { prepareImage = uploads.PrepareImage })
	if _, ok := mcpImage("image/png", pngImage, []string{"notes-secret-value"}); ok {
		t.Fatal("an image whose re-encoding holds a secret went")
	}
}

// At most maxMCPImages images of a result are decoded, whether each goes
// or not.
func TestFailedImagesCountAgainstTheBound(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	decoded := 0
	prepareImage = func([]byte) (uploads.Image, string, error) {
		decoded++
		return uploads.Image{}, "", errors.New("the pixels do not decode")
	}
	t.Cleanup(func() { prepareImage = uploads.PrepareImage })
	var content []mcp.ContentBlock
	for range 10 {
		content = append(content, mcp.ContentBlock{Type: "image", Data: pngImage, MimeType: "image/png"})
	}
	e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: content})
	if decoded != maxMCPImages {
		t.Fatalf("%d images decoded", decoded)
	}
}

// A schema's required names are each once: a provider refuses a list
// with one twice.
func TestRequiredNamesAreEachOnce(t *testing.T) {
	twice := `{"type":"object","properties":{"a":{"type":"string"}},"required":["a","a"]}`
	if got := string(normalSchema(json.RawMessage(twice))); got != emptySchema {
		t.Fatalf("kept %s", got)
	}
	once := `{"type":"object","properties":{"a":{"type":"string"},"b":{"type":"string"}},"required":["a","b"]}`
	if got := string(normalSchema(json.RawMessage(once))); got == emptySchema {
		t.Fatal("a schema with distinct required names was dropped")
	}
}

// A redirect is followed to its origin however it is spelled: a host's
// case, or its scheme's default port written out; never to another.
func TestARedirectToTheSameOriginIsFollowed(t *testing.T) {
	client := mcpClient(time.Second)
	from, _ := url.Parse(notesURL)
	via := []*http.Request{{URL: from}}
	for raw, follow := range map[string]bool{
		"https://MCP.example.com:443/notes/moved": true,
		"https://mcp.example.com/notes/moved":     true,
		"https://mcp.example.com:8443/notes":      false,
		"http://mcp.example.com/notes":            false,
		"https://other.example.com/notes":         false,
	} {
		to, _ := url.Parse(raw)
		err := client.CheckRedirect(&http.Request{URL: to}, via)
		if follow != (err == nil) {
			t.Errorf("%s: %v", raw, err)
		}
	}
}
