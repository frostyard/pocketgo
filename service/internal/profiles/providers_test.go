package profiles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const halogen = `{"providers": [{"id": "halogen", "name": "Halogen", "api": "openai-completions", "baseUrl": "http://halogen.example:8731/v1/",
	"models": [{"id": "halogen-qwen3.8-flash-next", "name": "Qwen3.8 Flash Next", "contextWindow": 262144, "maxTokens": 65536, "reasoning": true}]}]}`

func builtin(id string) bool { return id == "deepseek" || id == "anthropic" }

// The owner's providers are checked in code, every key as written
// (ADR-0023).
func TestParseProviders(t *testing.T) {
	got, err := ParseProviders([]byte(halogen), builtin)
	if err != nil || len(got) != 1 {
		t.Fatalf("got %+v, %v", got, err)
	}
	p := got[0]
	if p.ID != "halogen" || p.Name != "Halogen" || p.API != APIOpenAICompletions || p.BaseURL != "http://halogen.example:8731/v1" || p.Key ||
		len(p.Models) != 1 || p.Models[0] != (ProviderModel{ID: "halogen-qwen3.8-flash-next", Name: "Qwen3.8 Flash Next", ContextWindow: 262144, MaxTokens: 65536, Reasoning: true}) {
		t.Fatalf("provider %+v", p)
	}
	keyed, err := ParseProviders([]byte(`{"providers": [{"id": "lab", "api": "anthropic-messages", "baseUrl": "https://lab.example", "key": true,
		"models": [{"id": "Org/Model-7B", "contextWindow": 8192, "maxTokens": 8192, "images": true}]}]}`), builtin)
	if err != nil || keyed[0].Name != "lab" || !keyed[0].Key || keyed[0].Models[0].Name != "Org/Model-7B" || !keyed[0].Models[0].Images {
		t.Fatalf("keyed %+v, %v", keyed, err)
	}
	model := func(fields string) string {
		return `{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://lab:1/v1", "models": [{` + fields + `}]}]}`
	}
	provider := func(fields string) string {
		return `{"providers": [{` + fields + `, "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}]}]}`
	}
	for name, tc := range map[string]struct{ text, want string }{
		"no list":            {`{}`, "names no providers"},
		"an empty list":      {`{"providers": []}`, "names 0 providers"},
		"another field":      {`{"providers": [], "x": 1}`, `the field "x"`},
		"another case":       {`{"Providers": []}`, `the field "Providers"`},
		"the list twice":     {`{"providers": [], "providers": []}`, `names "providers" twice`},
		"trailing text":      {halogen + `{}`, "text follows"},
		"a built-in id":      {provider(`"id": "deepseek", "api": "openai-completions", "baseUrl": "http://x/v1"`), "pocketd's own"},
		"a bad id":           {provider(`"id": "Halogen", "api": "openai-completions", "baseUrl": "http://x/v1"`), "lowercase"},
		"an unknown api":     {provider(`"id": "lab", "api": "openai-responses", "baseUrl": "http://x/v1"`), "the api"},
		"no base url":        {provider(`"id": "lab", "api": "openai-completions"`), "the baseUrl"},
		"a relative url":     {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "/v1"`), "not http or https"},
		"another scheme":     {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "file:///v1"`), "not http or https"},
		"a user":             {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://me:pw@x/v1"`), "names a user"},
		"a query":            {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1?k=1"`), "query or a fragment"},
		"a fragment":         {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1#a"`), "query or a fragment"},
		"a provider field":   {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "apiKey": "sk"`), `the field "apiKey"`},
		"a field in caps":    {provider(`"ID": "lab", "api": "openai-completions", "baseUrl": "http://x/v1"`), `the field "ID"`},
		"an id twice":        {provider(`"id": "lab", "id": "other", "api": "openai-completions", "baseUrl": "http://x/v1"`), `names "id" twice`},
		"a key as text":      {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "key": "true"`), "cannot unmarshal"},
		"an id as a number":  {provider(`"id": 7, "api": "openai-completions", "baseUrl": "http://x/v1"`), "is not text"},
		"no models":          {`{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1"}]}`, "names 0 models"},
		"a model field":      {model(`"id": "m", "contextWindow": 10, "maxTokens": 5, "cost": 1`), `the field "cost"`},
		"a model id space":   {model(`"id": "m 1", "contextWindow": 10, "maxTokens": 5`), "holds a space"},
		"no context":         {model(`"id": "m", "maxTokens": 5`), "contextWindow 0"},
		"too much output":    {model(`"id": "m", "contextWindow": 10, "maxTokens": 11`), "maxTokens 11"},
		"a model twice":      {`{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}, {"id": "m", "contextWindow": 10, "maxTokens": 5}]}]}`, `"m" is named twice`},
		"a provider twice":   {`{"providers": [` + strings.Repeat(`{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}]},`, 2) + `]}`, ""},
		"a long name":        {provider(`"id": "lab", "name": "` + strings.Repeat("n", 61) + `", "api": "openai-completions", "baseUrl": "http://x/v1"`), "the name"},
		"not an object":      {`[]`, "is no object"},
		"providers as a map": {`{"providers": {}}`, "is no list"},
	} {
		_, err := ParseProviders([]byte(tc.text), builtin)
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
	twice := `{"providers": [` + strings.TrimSuffix(strings.Repeat(`{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}]},`, 2), ",") + `]}`
	if _, err := ParseProviders([]byte(twice), builtin); err == nil || !strings.Contains(err.Error(), `provider "lab" is named twice`) {
		t.Errorf("a provider twice: %v", err)
	}
}

// No file is no provider; a file in the folder is read only as a regular
// file of its own, as a profile is.
func TestLoadProviders(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadProviders(dir, builtin); err != nil || got != nil {
		t.Fatalf("no file: %+v, %v", got, err)
	}
	path := filepath.Join(dir, ProvidersFile)
	if err := os.WriteFile(path, []byte(halogen), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Folder{Dir: dir}).Providers(builtin); err != nil || len(got) != 1 || got[0].ID != "halogen" {
		t.Fatalf("the file: %+v, %v", got, err)
	}
	if err := os.WriteFile(path, []byte(`{"providers": []}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadProviders(dir, builtin); err == nil || !strings.Contains(err.Error(), ProvidersFile) {
		t.Fatalf("an invalid file: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "providers.json")
	if err := os.WriteFile(outside, []byte(halogen), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, path); err != nil {
		t.Fatal(err)
	}
	if got, err := LoadProviders(dir, builtin); err == nil {
		t.Fatalf("a link was read: %+v", got)
	}
}
