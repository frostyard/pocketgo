package profiles

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const halogen = `{"providers": [{"id": "halogen", "name": "Halogen", "api": "openai-completions", "baseUrl": "http://halogen.example:8731/v1/",
	"models": [{"id": "halogen-qwen3.8-flash-next", "name": "Qwen3.8 Flash Next", "contextWindow": 262144, "maxTokens": 65536, "reasoning": true}]}]}`

func builtin(id string) bool { return id == "deepseek" || id == "anthropic" }

// The owner's providers are checked in code, every key as written
// (ADR-0023).
func TestParseProviders(t *testing.T) {
	parsed, err := ParseProviders([]byte(halogen), builtin)
	got := parsed.Providers
	if err != nil || len(got) != 1 || parsed.Added != nil {
		t.Fatalf("got %+v, %v", parsed, err)
	}
	p := got[0]
	if p.ID != "halogen" || p.Name != "Halogen" || p.API != APIOpenAICompletions || p.BaseURL != "http://halogen.example:8731/v1" || p.Key ||
		len(p.Models) != 1 || p.Models[0] != (ProviderModel{ID: "halogen-qwen3.8-flash-next", Name: "Qwen3.8 Flash Next", ContextWindow: 262144, MaxTokens: 65536, Reasoning: true}) {
		t.Fatalf("provider %+v", p)
	}
	long := strings.Repeat("m", 100)
	parsed, err = ParseProviders([]byte(`{"providers": [{"id": "lab", "api": "anthropic-messages", "baseUrl": "https://lab.example", "key": true,
		"models": [{"id": "Org/Model-7B", "contextWindow": 8192, "maxTokens": 8192, "images": true}, {"id": "`+long+`", "contextWindow": 10, "maxTokens": 5}]}]}`), builtin)
	keyed := parsed.Providers
	if err != nil || keyed[0].Name != "lab" || !keyed[0].Key || keyed[0].Models[0].Name != "Org/Model-7B" || !keyed[0].Models[0].Images {
		t.Fatalf("keyed %+v, %v", keyed, err)
	}
	// A long id with no name is its own name: the name's limit holds for
	// a name written (Codex review of #34).
	if keyed[0].Models[1].Name != long {
		t.Fatalf("the long id's name %q", keyed[0].Models[1].Name)
	}
	model := func(fields string) string {
		return `{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://lab:1/v1", "models": [{` + fields + `}]}]}`
	}
	provider := func(fields string) string {
		return `{"providers": [{` + fields + `, "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}]}]}`
	}
	for name, tc := range map[string]struct{ text, want string }{
		"no list":       {`{}`, "names no providers and no builtinModels"},
		"an empty list": {`{"providers": []}`, "names 0 providers"},
		"another field": {`{"providers": [], "x": 1}`, `the field "x"`},
		"another case":  {`{"Providers": []}`, `the field "Providers"`},
		// A decoder takes null into a bool or a number and leaves it
		// unset (Codex review of #34).
		"a null key":         {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "key": null`), "not true or false"},
		"a null reasoning":   {model(`"id": "m", "contextWindow": 10, "maxTokens": 5, "reasoning": null`), "not true or false"},
		"a null context":     {model(`"id": "m", "contextWindow": null, "maxTokens": 5`), "contextWindow is not a whole number"},
		"a fractional size":  {model(`"id": "m", "contextWindow": 10.5, "maxTokens": 5`), "contextWindow is not a whole number"},
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
		"a key as text":      {provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "key": "true"`), "not true or false"},
		"an id as a number":  {provider(`"id": 7, "api": "openai-completions", "baseUrl": "http://x/v1"`), "is not text"},
		"no models":          {`{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1"}]}`, "names 0 models"},
		"a model field":      {model(`"id": "m", "contextWindow": 10, "maxTokens": 5, "cost": 1`), `the field "cost"`},
		"a model id space":   {model(`"id": "m 1", "contextWindow": 10, "maxTokens": 5`), "holds a space"},
		"no context":         {model(`"id": "m", "maxTokens": 5`), "contextWindow 0"},
		"too much output":    {model(`"id": "m", "contextWindow": 10, "maxTokens": 11`), "maxTokens 11"},
		"a model twice":      {`{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}, {"id": "m", "contextWindow": 10, "maxTokens": 5}]}]}`, "models 1 and 2 share an id"},
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
	// A refused URL is never repeated: it may hold a password or a
	// token, and pocketd logs the refusal (Codex review of #34).
	for _, secret := range []string{"http://me:hunter2@x/v1", "http://x/v1?key=hunter2", "http://x/v1#hunter2", "ht tp://hunter2"} {
		_, err := ParseProviders([]byte(provider(`"id": "lab", "api": "openai-completions", "baseUrl": "`+secret+`"`)), builtin)
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("a refused URL is repeated: %v", err)
		}
	}
	// A value written in the wrong place is never repeated either: a key
	// where true belongs, or a URL with a token in an object (Codex
	// review of #34, round 2).
	for name, text := range map[string]string{
		"a key for key":       provider(`"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "key": "sk-hunter2"`),
		"an object for a url": provider(`"id": "lab", "api": "openai-completions", "baseUrl": {"url": "http://x/v1?token=hunter2"}`),
		"a list for a url":    provider(`"id": "lab", "api": "openai-completions", "baseUrl": ["hunter2"]`),
		"a key for a size":    model(`"id": "m", "contextWindow": "sk-hunter2", "maxTokens": 5`),
		"a key for a name":    provider(`"id": "lab", "name": {"k": "hunter2"}, "api": "openai-completions", "baseUrl": "http://x/v1"`),
	} {
		_, err := ParseProviders([]byte(text), builtin)
		if err == nil || strings.Contains(err.Error(), "hunter2") {
			t.Errorf("%s: the value is repeated: %v", name, err)
		}
	}
	// Nor is a secret in a field of the right type whose value breaks a
	// rule: a URL in api, a key as an id or a name (Codex review of #34,
	// round 3).
	secret := "http://x/v1?token=hunter2"
	for name, text := range map[string]string{
		"a url in api":        provider(`"id": "lab", "api": "` + secret + `", "baseUrl": "http://x/v1"`),
		"a key as an id":      provider(`"id": "sk-Hunter2", "api": "openai-completions", "baseUrl": "http://x/v1"`),
		"a key as a name":     provider(`"id": "lab", "name": "hunter2 ` + strings.Repeat("x", 60) + `", "api": "openai-completions", "baseUrl": "http://x/v1"`),
		"a key as a model id": model(`"id": "sk hunter2", "contextWindow": 10, "maxTokens": 5`),
		"a key twice":         `{"providers": [{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "models": [{"id": "hunter2", "contextWindow": 10, "maxTokens": 5}, {"id": "hunter2", "contextWindow": 10, "maxTokens": 5}]}]}`,
		"a key as a provider": `{"builtinModels": [{"provider": "hunter2", "models": [{"id": "m", "like": "n"}]}]}`,
		"a bad provider":      `{"builtinModels": [{"provider": "Hunter2!", "models": [{"id": "m", "like": "n"}]}]}`,
	} {
		_, err := ParseProviders([]byte(text), builtin)
		if err == nil || strings.Contains(err.Error(), "hunter2") || strings.Contains(err.Error(), "Hunter2") {
			t.Errorf("%s: the value is repeated: %v", name, err)
		}
	}
	// Bytes that are no UTF-8 are refused, not replaced (Codex review of
	// #34, round 3).
	if _, err := ParseProviders([]byte(model("\"id\": \"m\xff1\", \"contextWindow\": 10, \"maxTokens\": 5")), builtin); err == nil || !strings.Contains(err.Error(), "not UTF-8") {
		t.Errorf("bytes that are no UTF-8: %v", err)
	}
	if _, err := ParseWorkers([]byte("{\"models\": [\"a/\xff\"]}")); err == nil || !strings.Contains(err.Error(), "not UTF-8") {
		t.Errorf("a worker list that is no UTF-8: %v", err)
	}
	twice := `{"providers": [` + strings.TrimSuffix(strings.Repeat(`{"id": "lab", "api": "openai-completions", "baseUrl": "http://x/v1", "models": [{"id": "m", "contextWindow": 10, "maxTokens": 5}]},`, 2), ",") + `]}`
	if _, err := ParseProviders([]byte(twice), builtin); err == nil || !strings.Contains(err.Error(), "providers 1 and 2 share an id") {
		t.Errorf("a provider twice: %v", err)
	}
}

// No file is no provider; a file in the folder is read only as a regular
// file of its own, as a profile is.
func TestLoadProviders(t *testing.T) {
	dir := t.TempDir()
	if got, err := LoadProviders(dir, builtin); err != nil || got.Providers != nil || got.Added != nil {
		t.Fatalf("no file: %+v, %v", got, err)
	}
	path := filepath.Join(dir, ProvidersFile)
	if err := os.WriteFile(path, []byte(halogen), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := (Folder{Dir: dir}).Providers(builtin); err != nil || len(got.Providers) != 1 || got.Providers[0].ID != "halogen" {
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

// Models added to one of PiG's own providers name the catalog's model they
// run as, and take their limits from it where they set none (ADR-0023).
func TestParseBuiltinModels(t *testing.T) {
	got, err := ParseProviders([]byte(`{"builtinModels": [{"provider": "anthropic", "models": [
		{"id": "claude-opus-5-6", "name": "Claude Opus 5.6", "like": "claude-opus-5-5", "contextWindow": 1000000, "maxTokens": 128000},
		{"id": "claude-haiku-5-6", "like": "claude-haiku-5-5"}]}]}`), builtin)
	want := []AddedModel{{ID: "claude-opus-5-6", Name: "Claude Opus 5.6", Like: "claude-opus-5-5", ContextWindow: 1000000, MaxTokens: 128000},
		{ID: "claude-haiku-5-6", Name: "claude-haiku-5-6", Like: "claude-haiku-5-5"}}
	if err != nil || got.Providers != nil || len(got.Added) != 1 || got.Added[0].Provider != "anthropic" || !slices.Equal(got.Added[0].Models, want) {
		t.Fatalf("got %+v, %v", got, err)
	}
	added := func(fields string) string {
		return `{"builtinModels": [{"provider": "anthropic", "models": [{` + fields + `}]}]}`
	}
	for name, tc := range map[string]struct{ text, want string }{
		"an empty list":     {`{"builtinModels": []}`, "name 0 providers"},
		"no models":         {`{"builtinModels": [{"provider": "anthropic"}]}`, "names 0 models"},
		"not pocketd's own": {`{"builtinModels": [{"provider": "lab", "models": [{"id": "m", "like": "n"}]}]}`, "none of pocketd's own providers"},
		"a provider twice":  {`{"builtinModels": [{"provider": "anthropic", "models": [{"id": "m", "like": "n"}]}, {"provider": "anthropic", "models": [{"id": "o", "like": "n"}]}]}`, "builtinModels 1 and 2 name one provider"},
		"no like":           {added(`"id": "m"`), "like, the catalog's model"},
		"like itself":       {added(`"id": "m", "like": "m"`), "like itself"},
		"a field":           {added(`"id": "m", "like": "n", "reasoning": true`), `the field "reasoning"`},
		"in capitals":       {added(`"ID": "m", "like": "n"`), `the field "ID"`},
		"a null size":       {added(`"id": "m", "like": "n", "maxTokens": null`), "maxTokens is not a whole number"},
		"too much output":   {added(`"id": "m", "like": "n", "contextWindow": 10, "maxTokens": 11`), "maxTokens 11"},
		"a model twice":     {`{"builtinModels": [{"provider": "anthropic", "models": [{"id": "m", "like": "n"}, {"id": "m", "like": "n"}]}]}`, "models 1 and 2 share an id"},
		"a provider entry":  {`{"builtinModels": [{"provider": "anthropic", "api": "x", "models": [{"id": "m", "like": "n"}]}]}`, `the field "api"`},
	} {
		if _, err := ParseProviders([]byte(tc.text), builtin); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: %v, want %q", name, err, tc.want)
		}
	}
}

// The folder pocketd pinned at start is the one its providers are read
// from: one put in its place since is refused (Codex review of #34).
func TestProvidersAreReadFromThePinnedFolder(t *testing.T) {
	parent := t.TempDir()
	dir := filepath.Join(parent, "agents")
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProvidersFile), []byte(halogen), 0o600); err != nil {
		t.Fatal(err)
	}
	folder, err := Pin(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := folder.Providers(builtin); err != nil || len(got.Providers) != 1 {
		t.Fatalf("the pinned folder: %+v, %v", got, err)
	}
	if err := os.Rename(dir, filepath.Join(parent, "old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ProvidersFile), []byte(halogen), 0o600); err != nil {
		t.Fatal(err)
	}
	if got, err := folder.Providers(builtin); err == nil {
		t.Fatalf("a folder put in its place was read: %+v", got)
	}
	// One put in its place after the check by name, before the open, is
	// refused too: the file is read through the handle checked (Codex
	// review of #34, round 2).
	if err := os.RemoveAll(dir); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(parent, "old"), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := folder.Providers(builtin); err != nil {
		t.Fatalf("the pinned folder, back: %v", err)
	}
	afterCheck = func() {
		afterCheck = nil
		if err := os.Rename(dir, filepath.Join(parent, "old")); err != nil {
			t.Error(err)
		}
		if err := os.Mkdir(dir, 0o700); err != nil {
			t.Error(err)
		}
		if err := os.WriteFile(filepath.Join(dir, ProvidersFile), []byte(halogen), 0o600); err != nil {
			t.Error(err)
		}
	}
	t.Cleanup(func() { afterCheck = nil })
	if got, err := folder.Providers(builtin); err == nil {
		t.Fatalf("a folder put in its place after the check was read: %+v", got)
	}
}

// A file of the pinned folder is read only as a regular file of its own:
// a link to another file in the folder is refused through the folder as by
// name (Codex review of #34, round 3). A folder that is gone is an error,
// never a missing file that would choose a default.
func TestThePinnedFolderReadsNoLinkAndNeedsTheFolder(t *testing.T) {
	dir := t.TempDir()
	for name, text := range map[string]string{"other.json": halogen, "list.json": `{"models": ["a/1"]}`} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(text), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Symlink("other.json", filepath.Join(dir, ProvidersFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("list.json", filepath.Join(dir, WorkersFile)); err != nil {
		t.Fatal(err)
	}
	folder, err := Pin(dir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if got, err := folder.Providers(builtin); err == nil {
		t.Fatalf("a linked providers file was read: %+v", got)
	}
	if got, err := folder.Workers(); err == nil {
		t.Fatalf("a linked worker list was read: %+v", got)
	}
	gone := Folder{Dir: filepath.Join(dir, "gone"), Home: t.TempDir()}
	if got, err := gone.Workers(); err == nil || got.Default {
		t.Fatalf("a folder that is gone chose the default list: %+v, %v", got, err)
	}
	if got, err := gone.Providers(builtin); err == nil {
		t.Fatalf("a folder that is gone read no file: %+v", got)
	}
	// The pinned folder itself gone: its check by name fails, and that is
	// no missing file either.
	pinnedDir := filepath.Join(t.TempDir(), "agents")
	if err := os.Mkdir(pinnedDir, 0o700); err != nil {
		t.Fatal(err)
	}
	pinned, err := Pin(pinnedDir, t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(pinnedDir); err != nil {
		t.Fatal(err)
	}
	if got, err := pinned.Workers(); err == nil || got.Default {
		t.Fatalf("a pinned folder that is gone chose the default list: %+v, %v", got, err)
	}
}
