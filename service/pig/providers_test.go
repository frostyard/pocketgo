package pig

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// fakeServer is an OpenAI-compatible server, as the owner's halogen is: it
// answers every chat completion with text, and keeps what it was asked.
type fakeServer struct {
	*httptest.Server
	mu       sync.Mutex
	paths    []string
	auth     []string
	apiKeys  []string
	bodies   []map[string]any
	response string
}

func newFakeServer(t *testing.T) *fakeServer {
	t.Helper()
	f := &fakeServer{response: "hello from lab"}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var decoded map[string]any
		_ = json.Unmarshal(body, &decoded)
		f.mu.Lock()
		f.paths = append(f.paths, r.URL.Path)
		f.auth = append(f.auth, r.Header.Get("Authorization"))
		f.apiKeys = append(f.apiKeys, r.Header.Get("X-Api-Key"))
		f.bodies = append(f.bodies, decoded)
		f.mu.Unlock()
		if r.URL.Path != "/v1/chat/completions" {
			http.Error(w, `{"error": {"message": "not this route", "type": "invalid_request_error"}}`, http.StatusNotFound)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		chunk := func(delta string, finish string) string {
			reason := "null"
			if finish != "" {
				reason = `"` + finish + `"`
			}
			return `data: {"id":"c1","object":"chat.completion.chunk","created":1,"model":"m1","choices":[{"index":0,"delta":` + delta + `,"finish_reason":` + reason + `}]}` + "\n\n"
		}
		_, _ = io.WriteString(w, chunk(`{"role":"assistant","content":"`+f.response+`"}`, ""))
		_, _ = io.WriteString(w, chunk(`{}`, "stop"))
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	t.Cleanup(f.Close)
	return f
}

func lab(base string, key bool, api string) profiles.Provider {
	return profiles.Provider{ID: "lab", Name: "Lab", API: api, BaseURL: base, Key: key,
		Models: []profiles.ProviderModel{{ID: "m1", Name: "Lab One", ContextWindow: 32768, MaxTokens: 4096, Reasoning: true}}}
}

func replyText(m *ai.AssistantMessage) string {
	var b strings.Builder
	for _, block := range m.Content {
		switch c := block.(type) {
		case ai.TextContent:
			b.WriteString(c.Text)
		case *ai.TextContent:
			b.WriteString(c.Text)
		}
	}
	return b.String()
}

func ask(t *testing.T, catalog *ai.Models, provider, id string) *ai.AssistantMessage {
	t.Helper()
	model := catalog.GetModel(provider, id)
	if model == nil {
		t.Fatalf("no model %s/%s", provider, id)
	}
	return catalog.CompleteSimple(context.Background(), model, ai.Context{Messages: []ai.Message{ai.UserMessage{Content: ai.UserText("Hi.")}}})
}

// The owner's provider, with no key, is listed among the models and runs
// a request on its base URL with PiG's OpenAI adapter, its model and no
// real key (ADR-0023).
func TestAnOwnersProviderServesItsModels(t *testing.T) {
	server := newFakeServer(t)
	path := filepath.Join(t.TempDir(), "auth.json")
	own := []profiles.Provider{lab(server.URL+"/v1", false, profiles.APIOpenAICompletions)}
	listed, err := ListModels(context.Background(), path, own)
	if err != nil || !slices.ContainsFunc(listed, func(m proto.Model) bool {
		return m.ModelRef == proto.ModelRef{Provider: "lab", ID: "m1"} && m.Name == "Lab One" && m.Reasoning
	}) {
		t.Fatalf("listed %+v, %v", listed, err)
	}
	catalog, _, err := signedIn(path, own)
	if err != nil {
		t.Fatal(err)
	}
	reply := ask(t, catalog, "lab", "m1")
	if reply.ErrorMessage != "" || replyText(reply) != "hello from lab" {
		t.Fatalf("reply %q, error %q", replyText(reply), reply.ErrorMessage)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.paths) != 1 || server.paths[0] != "/v1/chat/completions" || server.auth[0] != "Bearer "+noKey || server.bodies[0]["model"] != "m1" {
		t.Fatalf("the request: %v %v %v", server.paths, server.auth, server.bodies)
	}
}

// A provider that takes a key is listed only once its key is stored, and
// sends that key; one that takes none sends none, whatever is stored.
func TestAnOwnersProviderTakesItsStoredKey(t *testing.T) {
	server := newFakeServer(t)
	path := filepath.Join(t.TempDir(), "auth.json")
	own := []profiles.Provider{lab(server.URL+"/v1", true, profiles.APIOpenAICompletions)}
	if listed, err := ListModels(context.Background(), path, own); err != nil || len(listed) != 0 {
		t.Fatalf("listed with no key %+v, %v", listed, err)
	}
	if err := os.WriteFile(path, []byte(`{"lab":{"type":"api_key","key":"sk-lab"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if listed, err := ListModels(context.Background(), path, own); err != nil || len(listed) != 1 {
		t.Fatalf("listed with a key %+v, %v", listed, err)
	}
	catalog, _, err := signedIn(path, own)
	if err != nil {
		t.Fatal(err)
	}
	if reply := ask(t, catalog, "lab", "m1"); reply.ErrorMessage != "" {
		t.Fatalf("reply error %q", reply.ErrorMessage)
	}
	unkeyed := []profiles.Provider{lab(server.URL+"/v1", false, profiles.APIOpenAICompletions)}
	catalog, _, err = signedIn(path, unkeyed)
	if err != nil {
		t.Fatal(err)
	}
	if reply := ask(t, catalog, "lab", "m1"); reply.ErrorMessage != "" {
		t.Fatalf("reply error %q", reply.ErrorMessage)
	}
	server.mu.Lock()
	defer server.mu.Unlock()
	if !slices.Equal(server.auth, []string{"Bearer sk-lab", "Bearer " + noKey}) {
		t.Fatalf("keys sent %v", server.auth)
	}
}

// A provider on the Anthropic API posts to its base URL's /v1/messages,
// with its key in X-Api-Key.
func TestAnOwnersAnthropicProviderPostsMessages(t *testing.T) {
	server := newFakeServer(t)
	own := []profiles.Provider{lab(server.URL, false, profiles.APIAnthropicMessages)}
	catalog, _, err := signedIn(filepath.Join(t.TempDir(), "auth.json"), own)
	if err != nil {
		t.Fatal(err)
	}
	_ = ask(t, catalog, "lab", "m1")
	server.mu.Lock()
	defer server.mu.Unlock()
	if len(server.paths) == 0 || server.paths[0] != "/v1/messages" || server.apiKeys[0] != noKey || server.bodies[0]["model"] != "m1" {
		t.Fatalf("the request: %v %v %v", server.paths, server.apiKeys, server.bodies)
	}
}

// pocketd login keeps a key for an owner's provider that takes one, and
// refuses one that takes none; PiG's own ids are known (ADR-0023).
func TestLoginTakesAnOwnersProvidersKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	keyed := []profiles.Provider{lab("http://lab.example/v1", true, profiles.APIOpenAICompletions)}
	if err := Login(context.Background(), path, "lab", keyed, strings.NewReader("sk-lab\n"), io.Discard); err != nil {
		t.Fatal(err)
	}
	auth, err := ai.NewAuthStorage(path)
	if err != nil {
		t.Fatal(err)
	}
	if cred, ok, err := auth.GetRaw("lab"); err != nil || !ok || cred.Type != ai.CredentialAPIKey || cred.Key != "sk-lab" {
		t.Fatalf("the credential %+v %v %v", cred.Type, ok, err)
	}
	unkeyed := []profiles.Provider{lab("http://lab.example/v1", false, profiles.APIOpenAICompletions)}
	if err := Login(context.Background(), path, "lab", unkeyed, strings.NewReader("sk-other\n"), io.Discard); err == nil || !strings.Contains(err.Error(), "takes no key") {
		t.Fatalf("a provider that takes no key: %v", err)
	}
	if !Builtin("deepseek") || !Builtin("anthropic") || Builtin("lab") {
		t.Fatal("Builtin does not tell PiG's own providers")
	}
}
