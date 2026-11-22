package pig

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp/oauth"

	"github.com/frostyard/pocketgo/service/internal/profiles"
	"github.com/frostyard/pocketgo/service/internal/uploads"
)

// The guards of the Codex review of #126, round 7.

// loginPrompt runs pocketd mcp login for server on dir's store until it
// asks for the address the browser lands on, then gives it none, and
// returns what it printed.
func loginPrompt(t *testing.T, dir string, server profiles.MCPServer) string {
	t.Helper()
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
	return printed.String()
}

// keepClient keeps a client pocketd registered with authURL for the
// gateway, for scope, as an earlier pocketd mcp login did.
func keepClient(t *testing.T, dir, scope string) {
	t.Helper()
	store, err := openMCPStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	resource, _ := oauth.ParseURL(gatewayURL)
	client := oauth.OAuthClientInformation{ClientID: "client-old"}
	client.RedirectURIs, client.Scope = []string{profiles.DefaultMCPCallback}, scope
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{ServerURL: resource.Href(), ClientInformation: &client,
		Discovery: &oauth.OAuthDiscoveryState{AuthorizationServerURL: authURL}}); err != nil {
		t.Fatal(err)
	}
}

// pocketd mcp login runs its flow on the discovery it checked the kept
// client against: a second discovery that names another authorization
// server never gets the client kept for the first.
func TestMCPLoginRunsOnTheDiscoveryItChecked(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.moveAfter = 1 })
	dir := t.TempDir()
	keepClient(t, dir, "")
	server, _ := mcpServers(t).Find("gateway")
	printed := loginPrompt(t, dir, server)
	if strings.Contains(printed, otherAuthURL) || !strings.Contains(printed, authURL+"/authorize?") || !strings.Contains(printed, "client_id=client-old") {
		t.Fatalf("printed %s", printed)
	}
}

// pocketd mcp login keeps a client registered with the authorization
// server discovery names now for every scope it asks for: it registers
// none again.
func TestMCPLoginKeepsAClientRegisteredForTheScopes(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.scopes = []string{"notes:read", "admin"} })
	dir := t.TempDir()
	keepClient(t, dir, "notes:read admin")
	server, _ := mcpServers(t).Find("gateway")
	printed := loginPrompt(t, dir, server)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 0 || !strings.Contains(printed, "client_id=client-old") {
		t.Fatalf("registered %d: %s", fake.registered, printed)
	}
}

// A tool's description that is JSON shows no secret, whatever its
// escapes, as a result's text does not.
func TestAJSONDescriptionShowsNoSecret(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	escape := string(rune(92)) + "u"
	description := `{"note": "notes-` + escape + `0073ecret-value"}`
	tools := e.mcp.sanitize("notes", []mcpTool{{Name: "described", Description: description}})
	if len(tools) != 1 || strings.Contains(tools[0].Description, "ecret-value") {
		t.Fatalf("sanitized %+v", tools)
	}
}

// An answer past its bound ends its call alone: the connection, which
// other sessions' calls may be using, stays.
func TestAnAnswerPastItsBoundKeepsTheConnection(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	c := e.mcp.conns["notes"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	fake.set(func() { fake.big = maxMCPMessage + 1 })
	got := c.call(context.Background(), "big", map[string]any{})
	text := got.Output[0].(ai.TextContent).Text
	if got.IsError == nil || !*got.IsError || !strings.Contains(text, "nothing pocketd could read") {
		t.Fatalf("result %s", text)
	}
	c.mu.Lock()
	after := c.client
	c.mu.Unlock()
	if before == nil || after != before {
		t.Fatal("an answer past its bound closed the shared connection")
	}
}

// A sign-in the server refused that holds no refresh token needs the
// owner at once: pocketd tries no refresh, says to sign in again, and
// does not try the server again by itself.
func TestASignInWithNoRefreshTokenIsFinal(t *testing.T) {
	fake := newFakeMCP(t)
	timers := &fakeAfter{}
	mcpAfter = timers.after
	t.Cleanup(func() { mcpAfter = defaultMCPAfter })
	f := newFixture(t)
	seedSignIn(t, f.dataDir, "old-token", "")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"gateway"}})
	e.ConnectMCP(context.Background())
	timers.mu.Lock()
	scheduled := len(timers.runs)
	timers.mu.Unlock()
	if scheduled != 0 {
		t.Fatalf("%d retries scheduled", scheduled)
	}
	c := e.mcp.conns["gateway"]
	_, err := c.connect(context.Background(), false)
	if text := c.failure(err); !strings.Contains(text, "holds no refresh token") || !strings.Contains(text, "pocketd mcp login gateway") {
		t.Fatalf("the failure says %q (%v)", text, err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	for _, seen := range fake.seen {
		if seen == "POST "+authURL+"/token" {
			t.Fatal("pocketd asked for a refresh with no refresh token")
		}
	}
}

// A property named $ref, and a default or an example that holds one, are
// the server's data, not references: the schema is kept.
func TestAPropertyNamedRefIsNoReference(t *testing.T) {
	for name, schema := range map[string]string{
		"a property":  `{"type":"object","properties":{"$ref":{"type":"string"}},"required":["$ref"]}`,
		"a default":   `{"type":"object","properties":{"x":{"type":"object","default":{"$ref":"#/nowhere"}}}}`,
		"an example":  `{"type":"object","properties":{"x":{"type":"object","examples":[{"$ref":3}]}}}`,
		"a recursion": `{"type":"object","properties":{"child":{"$ref":"#"},"node":{"$ref":"#/$defs/node"}},"$defs":{"node":{"type":"object","properties":{"next":{"$ref":"#/$defs/node"}}}}}`,
	} {
		if got := string(normalSchema(json.RawMessage(schema))); got == emptySchema {
			t.Errorf("%s: the schema was dropped", name)
		}
	}
}

// A reference names a place of the schema read as a schema: one to a
// type, to properties' map, or to a default is offered as an object of
// any properties.
func TestAReferenceNamesASchema(t *testing.T) {
	for name, schema := range map[string]string{
		"a type":       `{"type":"object","properties":{"x":{"$ref":"#/type"}}}`,
		"a properties": `{"type":"object","properties":{"x":{"$ref":"#/properties"}}}`,
		"a default":    `{"type":"object","properties":{"x":{"type":"object","default":{"type":"string"}},"y":{"$ref":"#/properties/x/default"}}}`,
		"an encoding":  `{"type":"object","properties":{"x":{"$ref":"#/properties/%zz"}}}`,
	} {
		if got := string(normalSchema(json.RawMessage(schema))); got != emptySchema {
			t.Errorf("%s: kept %s", name, got)
		}
	}
	escaped := `{"type":"object","properties":{"a/b":{"type":"string"},"c":{"$ref":"#/properties/a~1b"},"d":{"$ref":"#/properties/a%7E1b"}}}`
	if got := string(normalSchema(json.RawMessage(escaped))); got == emptySchema {
		t.Error("a reference with an escape was dropped")
	}
}

// An image is within its bound as the model gets it: one that comes out
// of its re-encoding past maxMCPImageBytes is left out.
func TestAReencodedImagePastItsBoundIsLeftOut(t *testing.T) {
	prepareImage = func([]byte) (uploads.Image, string, error) {
		return uploads.Image{Data: make([]byte, maxMCPImageBytes), MimeType: "image/png"}, "", nil
	}
	t.Cleanup(func() { prepareImage = uploads.PrepareImage })
	if _, ok := mcpImage("image/png", pngImage, nil); ok {
		t.Fatal("an image past its bound once re-encoded went")
	}
}

// Two tools whose digest names are alike are neither offered, whatever
// their order: the name never reaches one in one listing and the other in
// the next. A digest is 16 hexadecimal digits.
func TestADigestCollisionOffersNeither(t *testing.T) {
	if name := mcpDigestName("notes", "find.note"); len(name) < 18 || name[len(name)-18:len(name)-16] != "__" {
		t.Fatalf("the digest name %s", name)
	}
	digest := mcpDigest
	mcpDigest = func(string, string) string { return "0000000000000000" }
	t.Cleanup(func() { mcpDigest = digest })
	long := strings.Repeat("a", 70)
	first, second := mcpTool{Name: long + ".one"}, mcpTool{Name: long + ".two"}
	for _, order := range [][]mcpTool{{first, second}, {second, first}} {
		names := mcpToolNames([]string{"notes"}, map[string][]mcpTool{"notes": order})
		if names["notes"][0] != "" || names["notes"][1] != "" {
			t.Fatalf("offered %q", names["notes"])
		}
	}
}
