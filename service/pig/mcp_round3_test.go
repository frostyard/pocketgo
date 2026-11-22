package pig

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"
	"github.com/MichaelKinsy/PiG/mcp/oauth"
)

// The guards of the Codex review of #126, round 3.

// A digest name is in no plain name's form: a tool whose own name is the
// tail of another's digest name, in another listing, never takes it.
func TestNoPlainNameIsADigestName(t *testing.T) {
	digest := mcpNamesOf("notes", "find.note")[0]
	tail := strings.TrimPrefix(digest, "mcp__notes__")
	if got := mcpNamesOf("notes", tail)[0]; got == digest {
		t.Fatalf("the tool %q took the digest name of find.note: %s", tail, got)
	}
	old := strings.TrimSuffix(tail, tail[len(tail)-10:]) + "_" + tail[len(tail)-8:]
	if got := mcpNamesOf("notes", old)[0]; got == digest {
		t.Fatalf("the tool %q took %s", old, got)
	}
}

// A result's image goes to the model only of a type the providers take,
// as base64 that decodes, and holding no secret the store keeps: any other
// is left out, never redacted inside.
func TestOnlyACheckedImageGoesToTheModel(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader("Bearer QUJDREVGR0hJSks=")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	c := e.mcp.conns["notes"]
	got := c.result(&mcp.CallToolResult{Content: []mcp.ContentBlock{
		{Type: "image", Data: "aGk=", MimeType: "image/png"},
		{Type: "image", Data: "QUJDREVGR0hJSks=", MimeType: "image/png"},
		{Type: "image", Data: "aGk=", MimeType: "image/png; QUJDREVGR0hJSks="},
		{Type: "image", Data: "aGk=", MimeType: "text/plain"},
		{Type: "image", Data: "not base64!", MimeType: "image/png"},
	}})
	images := 0
	for _, block := range got.Output {
		switch b := block.(type) {
		case ai.ImageContent:
			images++
			if b.Data != "aGk=" || b.MimeType != "image/png" {
				t.Fatalf("an image went %+v", b)
			}
		case ai.TextContent:
			if strings.Contains(b.Text, "QUJDREVGR0hJSks") {
				t.Fatalf("a text shows the secret: %s", b.Text)
			}
		}
	}
	if images != 1 {
		t.Fatalf("%d images went", images)
	}
}

// A secret the store replaced is still redacted: a call that went out
// with it may answer after a refresh replaced it.
func TestAReplacedSecretIsStillRedacted(t *testing.T) {
	store, err := openMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: "access-token-old", RefreshToken: "refresh-token-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.saveOAuth("gateway", gatewayURL, oauth.McpOAuthState{Tokens: &oauth.OAuthTokens{AccessToken: "access-token-new", RefreshToken: "refresh-token-1"}}); err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", "Bearer header-value-old"); err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", "Bearer header-value-new"); err != nil {
		t.Fatal(err)
	}
	got := redact("access-token-old header-value-old access-token-new", store.secrets())
	if got != "[redacted] [redacted] [redacted]" {
		t.Fatalf("redacted %q", got)
	}
}

// A tool list is read a page at a time, and pocketd stops at its bounds:
// a server with pages without end holds no more than maxMCPListPages of
// them.
func TestAToolListIsReadWithinItsBounds(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	fake.set(func() { fake.perPage = 1 })
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	e.ConnectMCP(context.Background())
	fake.mu.Lock()
	pages := fake.pages
	fake.mu.Unlock()
	if pages != maxMCPListPages || len(e.mcp.extension("chani").Tools) != maxMCPListPages {
		t.Fatalf("%d pages, %d tools", pages, len(e.mcp.extension("chani").Tools))
	}
	fake.set(func() { fake.perPage, fake.pages = 100, 0 })
	e.ConnectMCP(context.Background())
	fake.mu.Lock()
	pages = fake.pages
	fake.mu.Unlock()
	if pages != 2 || len(e.mcp.extension("chani").Tools) != maxProfileMCPTools {
		t.Fatalf("%d pages, %d tools", pages, len(e.mcp.extension("chani").Tools))
	}
}

// A call the server refuses for a scope pocketd's sign-in lacks says what
// the owner does, and keeps the shared connection.
func TestAScopeRefusalSaysWhatToDoAndKeepsTheConnection(t *testing.T) {
	fake := newFakeMCP(t)
	f := newFixture(t)
	fake.set(func() { fake.access["good-token"] = true })
	seedSignIn(t, f.dataDir, "good-token", "ref-1")
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"gateway"}})
	e.ConnectMCP(context.Background())
	c := e.mcp.conns["gateway"]
	c.mu.Lock()
	before := c.client
	c.mu.Unlock()
	got := c.call(context.Background(), "admin", map[string]any{})
	text := got.Output[0].(ai.TextContent).Text
	if got.IsError == nil || !*got.IsError || !strings.Contains(text, "a scope pocketd's sign-in lacks") || !strings.Contains(text, "pocketd mcp login gateway") {
		t.Fatalf("result %s", text)
	}
	c.mu.Lock()
	after := c.client
	c.mu.Unlock()
	if before == nil || after != before {
		t.Fatal("a scope refusal closed the shared connection")
	}
}

// A schema whose keywords hold no schema where one belongs (a property
// that is a string) is offered as an object of any properties: a provider
// would refuse every request that carries it.
func TestANestedSchemaIsChecked(t *testing.T) {
	for name, schema := range map[string]string{
		"a property string": `{"type":"object","properties":{"x":"string"}}`,
		"items a number":    `{"type":"object","properties":{"x":{"type":"array","items":3}}}`,
		"a bad type":        `{"type":"object","properties":{"x":{"type":"text"}}}`,
		"required a number": `{"type":"object","required":[1]}`,
		"anyOf no list":     `{"type":"object","properties":{"x":{"anyOf":{"type":"string"}}}}`,
	} {
		if got := string(normalSchema(json.RawMessage(schema))); got != emptySchema {
			t.Errorf("%s: kept %s", name, got)
		}
	}
	good := `{"type":"object","properties":{"x":{"type":["string","null"],"items":{"type":"string"},"default":{"type":3}}},"required":["x"]}`
	if got := string(normalSchema(json.RawMessage(good))); got == emptySchema {
		t.Errorf("a good schema was dropped")
	}
}

// A pasted address longer than its bound is refused, never cut.
func TestAnOverlongAddressIsRefused(t *testing.T) {
	if _, err := readLine(strings.NewReader(strings.Repeat("a", 20)+"\n"), 10); err == nil {
		t.Fatal("a line past its bound was read")
	}
	if line, err := readLine(strings.NewReader("abc\n"), 10); err != nil || line != "abc" {
		t.Fatalf("%q, %v", line, err)
	}
}

// A schema that spells a stored secret with JSON's escapes, as a kept
// listing may, is offered as an object of any properties: decoded, it is
// the secret.
func TestSanitizeFindsAnEscapedSecret(t *testing.T) {
	store, err := openMCPStore(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.setHeader("notes", notesURL, "Authorization", notesKey); err != nil {
		t.Fatal(err)
	}
	h := &mcpHub{store: store, log: discardLog()}
	raw := json.RawMessage(`{"type":"object","properties":{"k":{"type":"string","default":"notes-` + "\\u" + `0073ecret-value"}}}`)
	if strings.Contains(string(raw), "notes-secret-value") {
		t.Fatal("the fixture holds the secret unescaped")
	}
	got := h.sanitize("notes", []mcpTool{{Name: "escaped", InputSchema: raw}})
	if len(got) != 1 || string(got[0].InputSchema) != emptySchema {
		t.Fatalf("kept %s", got[0].InputSchema)
	}
}
