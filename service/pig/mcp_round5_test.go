package pig

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// The guards of the Codex review of #126, round 5.

// An image goes only when it decodes whole: a PNG's first bytes alone,
// whose signature reads well, are left out.
func TestATruncatedImageIsLeftOut(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	whole, _ := base64.StdEncoding.DecodeString(pngImage)
	truncated := base64.StdEncoding.EncodeToString(whole[:len(whole)/2])
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image", Data: truncated, MimeType: "image/png"},
		{Type: "image", Data: pngImage, MimeType: "image/png"},
	}})
	images := 0
	for _, block := range got.Output {
		if _, ok := block.(ai.ImageContent); ok {
			images++
		}
	}
	if images != 1 {
		t.Fatalf("%d images went", images)
	}
}

// Once the hub closed, the store writes nothing: a refresh that ends past
// pocketd's stop cannot write over what a later writer keeps.
func TestTheStoreWritesNothingOnceTheHubClosed(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	path := filepath.Join(f.dataDir, MCPAuthFile)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	e.mcp.close(context.Background())
	if err := e.mcp.store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: "late-token"}}); err == nil {
		t.Fatal("a write after close was taken")
	}
	after, err := os.ReadFile(path)
	if err != nil || string(after) != string(before) {
		t.Fatalf("the store's file changed after close: %v", err)
	}
	if !e.mcp.begin() {
		return
	}
	t.Fatal("a closed hub began an exchange")
}

// A text block that is JSON, written by an encoder that escapes a quote
// and not an ampersand, shows no secret: it is redacted decoded.
func TestJSONTextShowsNoSecretWhateverItsEscapes(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	const value = `Bearer abc"d&ef1234`
	f.keepHeader(value)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	text := `{"echo": "abc\"d&ef1234", "note": "ok"}`
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: text}}})
	out := got.Output[0].(ai.TextContent).Text
	var decoded map[string]string
	if err := json.Unmarshal([]byte(out), &decoded); err != nil || decoded["echo"] != "[redacted]" || decoded["note"] != "ok" {
		t.Fatalf("the result is %s (%v)", out, err)
	}
}

// A structured result is written compact before its bound: PiG's indented
// rendering of a deep value is never made.
func TestAStructuredResultIsWrittenCompact(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	deep := strings.Repeat("[", 200) + strings.Repeat("]", 200)
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{StructuredContent: json.RawMessage(deep)})
	out := got.Output[0].(ai.TextContent).Text
	if out != deep {
		t.Fatalf("the structured result is %.80q", out)
	}
}

// A listing on a live connection answered with a JSON-RPC error, or with
// no list, keeps the connection, and the last listing.
func TestAListErrorKeepsTheSharedConnection(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	ctx := context.Background()
	e.ConnectMCP(ctx)
	c := e.mcp.conns["notes"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	for _, change := range []func(){func() { fake.listError = true }, func() { fake.listError, fake.noList = false, true }} {
		fake.set(change)
		if _, err := c.connect(ctx, true); err == nil {
			t.Fatal("a failed listing succeeded")
		}
		c.mu.Lock()
		after := c.client
		c.mu.Unlock()
		if before == nil || after != before {
			t.Fatal("a failed listing closed the shared connection")
		}
	}
	if got := len(e.mcp.extension("chani").Tools); got != 2 {
		t.Fatalf("%d tools left", got)
	}
}

// pocketd mcp login decides whether to keep a client from the scopes the
// request will name, the resource metadata's when nobody names any.
func TestMCPLoginKeepsAClientOnlyForTheMetadatasScopes(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.scopes = []string{"notes:read", "admin"} })
	dir := t.TempDir()
	server, _ := mcpServers(t).Find("gateway")
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resource, _ := oauth.ParseURL(gatewayURL)
	client := oauth.OAuthClientInformation{ClientID: "client-old"}
	client.RedirectURIs, client.Scope = []string{profiles.DefaultMCPCallback}, "notes:read"
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{ServerURL: resource.Href(), ClientInformation: &client}); err != nil {
		t.Fatal(err)
	}
	inR, inW := io.Pipe()
	outR, outW := io.Pipe()
	done := make(chan error, 1)
	go func() {
		done <- MCPLogin(context.Background(), dir, server, inR, outW)
		_ = outW.Close()
	}()
	var printed strings.Builder
	buf := make([]byte, 4096)
	for !strings.Contains(printed.String(), "and Enter:") {
		n, err := outR.Read(buf)
		if err != nil {
			t.Fatalf("read the prompt: %v (%s)", err, printed.String())
		}
		printed.Write(buf[:n])
	}
	_ = inW.Close()
	_, _ = io.ReadAll(outR)
	<-done
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 1 {
		t.Fatalf("registered %d: %s", fake.registered, printed.String())
	}
}

// A schema's standard constraints are checked for their kind: a number
// where a reference belongs, text where a length belongs, or a reference
// to another document, offer an object of any properties.
func TestASchemasConstraintsAreChecked(t *testing.T) {
	for name, schema := range map[string]string{
		"a numeric ref":    `{"type":"object","properties":{"x":{"$ref":3}}}`,
		"a remote ref":     `{"type":"object","properties":{"x":{"$ref":"https://example.com/s.json"}}}`,
		"a text minLength": `{"type":"object","properties":{"x":{"type":"string","minLength":"2"}}}`,
		"a negative max":   `{"type":"object","properties":{"x":{"type":"array","maxItems":-1}}}`,
		"a zero multiple":  `{"type":"object","properties":{"x":{"type":"number","multipleOf":0}}}`,
		"a text bool":      `{"type":"object","properties":{"x":{"type":"array","uniqueItems":"yes"}}}`,
	} {
		if got := string(normalSchema(json.RawMessage(schema))); got != emptySchema {
			t.Errorf("%s: kept %s", name, got)
		}
	}
	good := `{"type":"object","properties":{"x":{"type":"string","minLength":1,"pattern":"^a","$ref":"#/$defs/x"}},"$defs":{"x":{"type":"string"}}}`
	if got := string(normalSchema(json.RawMessage(good))); got == emptySchema {
		t.Error("a good schema was dropped")
	}
}

// A refusal names only an OAuth code it knows: a token put where a code
// belongs is never repeated.
func TestARefusalNamesOnlyAKnownOAuthCode(t *testing.T) {
	if got := safeWord("access_denied"); got != "access_denied" {
		t.Errorf("a known code: %s", got)
	}
	if got := safeWord("sk-live-token-1234"); strings.Contains(got, "sk-live") {
		t.Errorf("an unknown code: %s", got)
	}
	cb := profiles.DefaultMCPCallback
	if _, _, err := callbackCode(cb+"?error=secretcode1234&state=s", cb, "s"); err == nil || strings.Contains(err.Error(), "secretcode1234") {
		t.Errorf("a pasted error: %v", err)
	}
}
