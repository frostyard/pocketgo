package pig

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"runtime"
	"strings"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"
	"github.com/MichaelKinsy/PiG/mcp"
)

// The guards of the Codex review of #126, round 10.

// pocketd mcp login asks for no scope that holds a secret pocketd keeps:
// a server may echo a stored header in its challenge, and the scope goes
// to the authorization server and on the printed page.
func TestMCPLoginRefusesAScopeHoldingASecret(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.challengeScope = "notes-secret-value" })
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
	if err == nil || strings.Contains(out.String(), "notes-secret-value") || strings.Contains(err.Error(), "notes-secret-value") {
		t.Fatalf("the sign-in went on (%v): %s", err, out.String())
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if fake.registered != 0 {
		t.Fatalf("registered %d", fake.registered)
	}
}

// A page of many small tools keeps only what pocketd takes, and decodes
// no more than it keeps: the cost of a page is its bytes, never a tool a
// listed entry. Its next cursor is read wherever it comes.
func TestADecodedPageKeepsOnlyWhatPocketdTakes(t *testing.T) {
	var page strings.Builder
	page.WriteString(`{"tools":[`)
	for i := range 50_000 {
		if i > 0 {
			page.WriteString(",")
		}
		page.WriteString(`{"name":"t"}`)
	}
	page.WriteString(`],"nextCursor":"more"}`)
	raw := json.RawMessage(page.String())
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	tools, next, err := decodeToolsPage(raw, 3)
	runtime.ReadMemStats(&after)
	if err != nil || len(tools) != 3 || next == nil || *next != "more" {
		t.Fatalf("decoded %d tools, next %v, %v", len(tools), next, err)
	}
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > 3*uint64(len(raw)) {
		t.Fatalf("a page of %d bytes allocated %d bytes", len(raw), allocated)
	}
	if _, _, err := decodeToolsPage(json.RawMessage(`{"nextCursor":"x"}`), 3); err == nil {
		t.Fatal("a page with no tools list is a list")
	}
	if _, _, err := decodeToolsPage(json.RawMessage(`{"tools":[{"name":"a"},3]}`), 1); err == nil {
		t.Fatal("a page with an entry that is no tool is a list")
	}
}

// A Bearer challenge is found after another scheme's, a comma in a quoted
// value never starting one: pocketd mcp login asks for its scope.
func TestALoginFindsTheBearerChallengeAfterAnother(t *testing.T) {
	fake := newFakeMCP(t)
	fake.set(func() { fake.basicFirst, fake.challengeScope = true, "notes:write" })
	server, _ := mcpServers(t).Find("gateway")
	printed := loginPrompt(t, t.TempDir(), server)
	found := false
	for line := range strings.SplitSeq(printed, "\n") {
		if u, err := url.Parse(strings.TrimSpace(line)); err == nil && strings.HasPrefix(strings.TrimSpace(line), authURL+"/authorize") {
			found = true
			if got := u.Query().Get("scope"); got != "notes:write" {
				t.Fatalf("scope %q", got)
			}
		}
	}
	if !found {
		t.Fatalf("no page: %s", printed)
	}
	header := http.Header{}
	header.Add("WWW-Authenticate", `Basic realm="a, Bearer scope=wrong", DPoP scope="right"`)
	if got := oauthChallenge(header); got.Scope != "right" {
		t.Fatalf("challenge %+v", got)
	}
}

// A schema whose pattern, or a patternProperties key, is no regular
// expression is offered as an object of any properties: PiG could not
// check a call's arguments against it.
func TestAPatternThatDoesNotCompileIsDropped(t *testing.T) {
	for name, schema := range map[string]string{
		"a pattern":           `{"type":"object","properties":{"a":{"type":"string","pattern":"[a-"}}}`,
		"a patternProperties": `{"type":"object","patternProperties":{"(":{"type":"string"}}}`,
	} {
		if got := string(normalSchema(json.RawMessage(schema))); got != emptySchema {
			t.Errorf("%s: kept %s", name, got)
		}
	}
	good := `{"type":"object","properties":{"a":{"type":"string","pattern":"^[a-z]+$"}}}`
	if got := string(normalSchema(json.RawMessage(good))); got == emptySchema {
		t.Fatal("a schema with a pattern that compiles was dropped")
	}
}

// A text that fits the bound goes whole, with no mark of a cut: the
// notices are kept room for only when they are said.
func TestATextThatFitsIsNotCut(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	text := strings.Repeat("x", maxMCPOutput)
	got := e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: []mcp.ContentBlock{{Type: "text", Text: text}}})
	if out := got.Output[0].(ai.TextContent).Text; out != text {
		t.Fatalf("a text of %d bytes came out as %d bytes", len(text), len(out))
	}
}

// No text past the bound is redacted: none of it goes.
func TestNoTextPastTheBoundIsRedacted(t *testing.T) {
	newFakeMCP(t)
	f := newFixture(t)
	f.keepHeader(notesKey)
	e := f.openMCP(fauxModels(), mcpServers(t), map[string][]string{"chani": {"notes"}})
	redacted := 0
	redactResultText = func(text string, secrets []string) string {
		redacted++
		return redactText(text, secrets)
	}
	t.Cleanup(func() { redactResultText = redactText })
	content := []mcp.ContentBlock{{Type: "text", Text: strings.Repeat("x", maxMCPOutput+1)}}
	for range 5 {
		content = append(content, mcp.ContentBlock{Type: "text", Text: "more"})
	}
	e.mcp.conns["notes"].result(&mcp.CallToolResult{Content: content})
	if redacted != 1 {
		t.Fatalf("%d texts redacted", redacted)
	}
}
