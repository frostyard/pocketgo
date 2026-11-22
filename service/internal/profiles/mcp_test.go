package profiles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// The owner's MCP servers are checked in code (ADR-0063): Pi's shape,
// servers by name with url, headers and oauth, and no secret in it.
func TestParseMCP(t *testing.T) {
	got, err := ParseMCP([]byte(`{"mcpServers": {
		"notes": {"url": "https://mcp.example.com/notes", "headers": {"authorization": "stored", "X-Agent": "stored"}},
		"gateway": {"type": "http", "url": "https://mcp.example.com/gateway/mcp", "oauth": {"scope": "notes:read mail:send"}, "timeout": 120},
		"local": {"url": "http://127.0.0.1:9000/mcp", "oauth": {"clientId": "pocketd", "callbackUrl": "http://localhost:7777/cb"}}
	}}`))
	if err != nil {
		t.Fatal(err)
	}
	names := []string{}
	for _, s := range got.Servers {
		names = append(names, s.Name)
	}
	if !slices.Equal(names, []string{"gateway", "local", "notes"}) {
		t.Fatalf("servers %v, want them by name", names)
	}
	notes, _ := got.Find("notes")
	if notes.URL != "https://mcp.example.com/notes" || !slices.Equal(notes.Headers, []string{"Authorization", "X-Agent"}) || notes.OAuth != nil || notes.Timeout != DefaultMCPTimeout {
		t.Errorf("notes: %+v", notes)
	}
	gateway, _ := got.Find("gateway")
	if gateway.OAuth == nil || gateway.OAuth.Scope != "notes:read mail:send" || gateway.OAuth.CallbackURL != DefaultMCPCallback || gateway.Timeout != 120*time.Second {
		t.Errorf("gateway: %+v %+v", gateway, gateway.OAuth)
	}
	local, _ := got.Find("local")
	if local.OAuth == nil || local.OAuth.ClientID != "pocketd" || local.OAuth.CallbackURL != "http://localhost:7777/cb" {
		t.Errorf("local: %+v", local.OAuth)
	}
	if _, ok := got.Find("mail"); ok {
		t.Error("found a server the file does not name")
	}
	tailnet := `{"mcpServers": {"a": {"url": "http://host.example.ts.net/mcp"}}}`
	if _, err := ParseMCP([]byte(tailnet)); err != nil {
		t.Errorf("plain HTTP to a tailnet name: %v", err)
	}
}

// A refusal says what to do, and never repeats a value: one may be the
// secret itself, and pocketd logs the refusal.
func TestParseMCPRefuses(t *testing.T) {
	const secret = "sk-secret-value"
	seventeen := `{"mcpServers": {`
	for i := range 17 {
		if i > 0 {
			seventeen += ","
		}
		seventeen += `"s` + string(rune('a'+i)) + `": {"url": "https://mcp.example.com/"}`
	}
	seventeen += `}}`
	for name, tc := range map[string]struct{ text, want string }{
		"no servers":       {`{"mcpServers": {}}`, "names 0 servers"},
		"no mcpServers":    {`{}`, "names 0 servers"},
		"too many":         {seventeen, "names 17 servers"},
		"another field":    {`{"mcpServers": {"a": {"url": "https://mcp.example.com/"}}, "autoEnableCodemode": true}`, `the field "autoEnableCodemode"`},
		"a header's value": {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "headers": {"Authorization": "Bearer ` + secret + `"}}}}`, "pocketd mcp set-header a Authorization"},
		"a stdio server":   {`{"mcpServers": {"a": {"command": "npx", "args": ["server"]}}}`, "HTTP servers alone"},
		"a provider auth":  {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "auth": {"provider": "anthropic"}}}}`, "no model provider's sign-in"},
		"a client secret":  {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "oauth": {"clientSecret": "` + secret + `"}}}}`, "a secret does not go in the profile folder"},
		"no url":           {`{"mcpServers": {"a": {"headers": {"X": "stored"}}}}`, "names no url"},
		"http elsewhere":   {`{"mcpServers": {"a": {"url": "http://mcp.example.com/"}}}`, "use https"},
		"a user":           {`{"mcpServers": {"a": {"url": "https://u:` + secret + `@mcp.example.com/"}}}`, "a user or a password"},
		"a query":          {`{"mcpServers": {"a": {"url": "https://mcp.example.com/?token=` + secret + `"}}}`, "a query or a fragment"},
		"a fragment":       {`{"mcpServers": {"a": {"url": "https://mcp.example.com/#` + secret + `"}}}`, "a query or a fragment"},
		"ftp":              {`{"mcpServers": {"a": {"url": "ftp://mcp.example.com/"}}}`, "not https"},
		"a bad port":       {`{"mcpServers": {"a": {"url": "https://mcp.example.com:70000/"}}}`, "port"},
		"a bad name":       {`{"mcpServers": {"no spaces": {"url": "https://mcp.example.com/"}}}`, "the name is not"},
		"a long name":      {`{"mcpServers": {"` + strings.Repeat("a", MaxMCPName+1) + `": {"url": "https://mcp.example.com/"}}}`, "the name is not"},
		"names alike":      {`{"mcpServers": {"a-b": {"url": "https://mcp.example.com/"}, "a_b": {"url": "https://mcp.example.com/"}}}`, "would name their tools alike"},
		"a reserved one":   {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "headers": {"Mcp-Session-Id": "stored"}}}}`, "sets itself"},
		"a bad header":     {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "headers": {"No Spaces": "stored"}}}}`, "not an HTTP header name"},
		"a header twice":   {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "headers": {"x-a": "stored", "X-A": "stored"}}}}`, "names the header X-A twice"},
		"oauth and a key":  {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "oauth": {}, "headers": {"Authorization": "stored"}}}}`, "both oauth and an Authorization header"},
		"a long timeout":   {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "timeout": 601}}}`, "not 1 to 600"},
		"a zero timeout":   {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "timeout": 0}}}`, "not above 0"},
		"a stdio type":     {`{"mcpServers": {"a": {"type": "stdio", "url": "https://mcp.example.com/"}}}`, "HTTP servers alone"},
		"a remote cb":      {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "oauth": {"callbackUrl": "https://example.com/cb"}}}}`, "loopback host"},
		"a cb with query":  {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "oauth": {"callbackUrl": "http://127.0.0.1/cb?x=1"}}}}`, "a query"},
		"an oauth field":   {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "oauth": {"callbackPort": 8080}}}}`, `the field "callbackPort"`},
		"a scope control":  {`{"mcpServers": {"a": {"url": "https://mcp.example.com/", "oauth": {"scope": "a\nb"}}}}`, "not printable"},
		"another case":     {`{"mcpServers": {"a": {"URL": "https://mcp.example.com/"}}}`, `the field "URL"`},
		"named twice":      {`{"mcpServers": {"a": {"url": "https://mcp.example.com/"}, "a": {"url": "https://mcp.example.com/"}}}`, `names "a" twice`},
		"trailing text":    {`{"mcpServers": {"a": {"url": "https://mcp.example.com/"}}} {}`, "text follows"},
		"not an object":    {`[]`, "no object"},
		"not UTF-8":        {"{\"mcpServers\": {\"a\": {\"url\": \"https://\xff/\"}}}", "not UTF-8"},
	} {
		_, err := ParseMCP([]byte(tc.text))
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
			continue
		}
		if strings.Contains(err.Error(), secret) {
			t.Errorf("%s: the refusal repeats the secret: %v", name, err)
		}
	}
}

// No file is no server; a file is read only as a regular file of its own
// in the folder, as a profile is.
func TestLoadMCP(t *testing.T) {
	dir := t.TempDir()
	got, err := LoadMCP(dir)
	if err != nil || len(got.Servers) != 0 {
		t.Fatalf("no file: %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, MCPFile), []byte(`{"mcpServers": {"a": {"url": "https://mcp.example.com/"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadMCP(dir); err != nil || len(got.Servers) != 1 {
		t.Fatalf("a file: %+v, %v", got, err)
	}
	if err := os.WriteFile(filepath.Join(dir, MCPFile), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMCP(dir); err == nil || !strings.Contains(err.Error(), MCPFile) {
		t.Errorf("a bad file: %v", err)
	}
	other := t.TempDir()
	target := filepath.Join(other, "real.json")
	if err := os.WriteFile(target, []byte(`{"mcpServers": {"a": {"url": "https://mcp.example.com/"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := t.TempDir()
	if err := os.Symlink(target, filepath.Join(link, MCPFile)); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadMCP(link); err == nil {
		t.Error("a link was read")
	}
	if _, err := LoadMCP(filepath.Join(dir, "gone")); err == nil {
		t.Error("a missing folder read as no file")
	}
}

// The repository's example mcp.json is one pocketd takes, with
// placeholder hosts alone.
func TestTheExampleMCPFileIsValid(t *testing.T) {
	got, err := LoadMCP(filepath.Join("..", "..", "..", "examples", "profiles"))
	if err != nil || len(got.Servers) != 3 {
		t.Fatalf("%+v %v", got, err)
	}
	for _, s := range got.Servers {
		if !strings.HasPrefix(s.URL, "https://mcp.example.com/") {
			t.Errorf("%s names a host that is no placeholder", s.Name)
		}
	}
}

// The profiles pocketd takes MCP grants from are read through the handle
// of the folder it pinned: a folder put at its name after the check is
// never read, as mcp.json is not (Codex review of #126, round 6).
func TestLoadPinnedReadsThroughTheHandleItChecked(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "agents")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "chani.md"), []byte("---\nmodel: a/b\nmcp: [notes]\n---\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	folder, err := Pin(dir, parent)
	if err != nil {
		t.Fatal(err)
	}
	got, problems, err := folder.LoadPinned()
	if err != nil || len(got) != 1 || len(problems) != 0 || got[0].MCP[0] != "notes" {
		t.Fatalf("%+v %+v %v", got, problems, err)
	}
	afterCheck = func() {
		afterCheck = nil
		if err := os.Rename(dir, filepath.Join(parent, "old")); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "vera.md"), []byte("---\nmodel: a/b\nmcp: [mail]\n---\n"), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterCheck = nil })
	if got, _, err := folder.LoadPinned(); err == nil {
		t.Fatalf("a folder put in its place after the check was read: %+v", got)
	}
}

// A server's name reads in its tools' names with every character but a
// letter, a digit or _ made _.
func TestMCPNamespace(t *testing.T) {
	for in, want := range map[string]string{"notes": "notes", "notes-main": "notes_main", "a.b_c": "a_b_c", "A1": "A1"} {
		if got := MCPNamespace(in); got != want {
			t.Errorf("MCPNamespace(%q) = %q, want %q", in, got, want)
		}
	}
}
