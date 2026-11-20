package profiles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProvidersFile is the owner's own model providers, in the profile folder,
// which agents' commands see read-only (ADR-0023).
const ProvidersFile = "providers.json"

// The bounds of the owner's providers (ADR-0023).
const (
	MaxProviders      = 8
	MaxProviderModels = 32
	maxProviderName   = 60
	maxModelID        = 128
	maxContextWindow  = 10_000_000
)

// The APIs an owner's provider speaks: PiG's adapters pocketd links.
const (
	APIOpenAICompletions = "openai-completions"
	APIAnthropicMessages = "anthropic-messages"
)

// Provider is one of the owner's own model providers, as checked.
type Provider struct {
	ID, Name, API, BaseURL string
	// Key says the provider takes a key, which the owner stores with
	// pocketd login: its models are listed only once one is stored.
	Key    bool
	Models []ProviderModel
}

// ProviderModel is one model of an owner's provider.
type ProviderModel struct {
	ID, Name                 string
	ContextWindow, MaxTokens int
	Reasoning, Images        bool
}

var providerID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// LoadProviders reads the owner's providers in dir: none when there is no
// file. builtin says whether an id names one of PiG's own providers. A
// file that is no regular file of its own, or breaks a rule of
// ParseProviders, is an error: none of its providers is registered.
func LoadProviders(dir string, builtin func(id string) bool) ([]Provider, error) {
	data, err := readProfile(filepath.Join(dir, ProvidersFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ProvidersFile, err)
	}
	providers, err := ParseProviders(data, builtin)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", ProvidersFile, err)
	}
	return providers, nil
}

// ParseProviders checks the owner's providers (ADR-0023). Every object's
// keys are read as written, since a decoder matches a key in any case and
// keeps the last of one named twice: another case, a key named twice or
// an unknown key is refused.
func ParseProviders(data []byte, builtin func(id string) bool) ([]Provider, error) {
	if len(data) > maxProfileSize {
		return nil, fmt.Errorf("the file is larger than %d bytes", maxProfileSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var providers []Provider
	seenList := false
	err := readObject(dec, "the file", func(key string) error {
		if key != "providers" {
			return fmt.Errorf("the file has the field %q, not only \"providers\"", key)
		}
		seenList = true
		return readArray(dec, "providers", func() error {
			p, err := readProvider(dec, len(providers)+1)
			if err != nil {
				return err
			}
			providers = append(providers, p)
			return nil
		})
	})
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("text follows the file's object")
	}
	switch {
	case !seenList:
		return nil, errors.New(`it names no providers: {"providers": [{…}]}`)
	case len(providers) == 0 || len(providers) > MaxProviders:
		return nil, fmt.Errorf("it names %d providers, not 1 to %d", len(providers), MaxProviders)
	}
	ids := map[string]bool{}
	for _, p := range providers {
		if builtin != nil && builtin(p.ID) {
			return nil, fmt.Errorf("provider %q is one of pocketd's own: give yours another id", p.ID)
		}
		if ids[p.ID] {
			return nil, fmt.Errorf("provider %q is named twice", p.ID)
		}
		ids[p.ID] = true
	}
	return providers, nil
}

// readProvider reads and checks provider number n of the list.
func readProvider(dec *json.Decoder, n int) (Provider, error) {
	var p Provider
	var hasModels bool
	where := fmt.Sprintf("provider %d", n)
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "id":
			return decodeString(dec, &p.ID)
		case "name":
			return decodeString(dec, &p.Name)
		case "api":
			return decodeString(dec, &p.API)
		case "baseUrl":
			return decodeString(dec, &p.BaseURL)
		case "key":
			return dec.Decode(&p.Key)
		case "models":
			hasModels = true
			return readArray(dec, where+"'s models", func() error {
				m, err := readModel(dec, fmt.Sprintf("%s's model %d", where, len(p.Models)+1))
				if err != nil {
					return err
				}
				p.Models = append(p.Models, m)
				return nil
			})
		}
		return fmt.Errorf("%s has the field %q: it takes id, name, api, baseUrl, key and models", where, key)
	})
	if err != nil {
		return Provider{}, err
	}
	if !providerID.MatchString(p.ID) {
		return Provider{}, fmt.Errorf("%s: the id %q is not lowercase letters, digits and dashes, starting with a letter, at most 32", where, p.ID)
	}
	where = fmt.Sprintf("provider %q", p.ID)
	if p.Name == "" {
		p.Name = p.ID
	}
	if err := checkName(p.Name, maxProviderName); err != nil {
		return Provider{}, fmt.Errorf("%s: the name %w", where, err)
	}
	if p.API != APIOpenAICompletions && p.API != APIAnthropicMessages {
		return Provider{}, fmt.Errorf("%s: the api %q is not %s or %s", where, p.API, APIOpenAICompletions, APIAnthropicMessages)
	}
	base, err := checkBaseURL(p.BaseURL)
	if err != nil {
		return Provider{}, fmt.Errorf("%s: the baseUrl %w", where, err)
	}
	p.BaseURL = base
	if !hasModels || len(p.Models) == 0 || len(p.Models) > MaxProviderModels {
		return Provider{}, fmt.Errorf("%s names %d models, not 1 to %d", where, len(p.Models), MaxProviderModels)
	}
	ids := map[string]bool{}
	for _, m := range p.Models {
		if ids[m.ID] {
			return Provider{}, fmt.Errorf("%s: the model %q is named twice", where, m.ID)
		}
		ids[m.ID] = true
	}
	return p, nil
}

// readModel reads and checks one model of a provider.
func readModel(dec *json.Decoder, where string) (ProviderModel, error) {
	var m ProviderModel
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "id":
			return decodeString(dec, &m.ID)
		case "name":
			return decodeString(dec, &m.Name)
		case "contextWindow":
			return dec.Decode(&m.ContextWindow)
		case "maxTokens":
			return dec.Decode(&m.MaxTokens)
		case "reasoning":
			return dec.Decode(&m.Reasoning)
		case "images":
			return dec.Decode(&m.Images)
		}
		return fmt.Errorf("%s has the field %q: it takes id, name, contextWindow, maxTokens, reasoning and images", where, key)
	})
	if err != nil {
		return ProviderModel{}, err
	}
	if m.ID == "" || len(m.ID) > maxModelID || strings.ContainsFunc(m.ID, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || !utf8.ValidString(m.ID) {
		return ProviderModel{}, fmt.Errorf("%s: the id %q is empty, longer than %d bytes, or holds a space", where, m.ID, maxModelID)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if err := checkName(m.Name, maxProviderName); err != nil {
		return ProviderModel{}, fmt.Errorf("%s: the name %w", where, err)
	}
	if m.ContextWindow < 1 || m.ContextWindow > maxContextWindow {
		return ProviderModel{}, fmt.Errorf("%s: the contextWindow %d is not 1 to %d", where, m.ContextWindow, maxContextWindow)
	}
	if m.MaxTokens < 1 || m.MaxTokens > m.ContextWindow {
		return ProviderModel{}, fmt.Errorf("%s: the maxTokens %d is not 1 to the contextWindow", where, m.MaxTokens)
	}
	return m, nil
}

// checkName refuses a name that is too long or holds a control character.
func checkName(name string, limit int) error {
	if utf8.RuneCountInString(name) > limit || !utf8.ValidString(name) || strings.ContainsFunc(name, unicode.IsControl) {
		return fmt.Errorf("%q is longer than %d characters or holds a control character", name, limit)
	}
	return nil
}

// checkBaseURL is an absolute http or https URL with a host, and no user,
// query or fragment, without a slash at its end: PiG joins its path to it.
func checkBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case err != nil:
		return "", fmt.Errorf("%q is no URL: %w", raw, err)
	case u.Scheme != "http" && u.Scheme != "https":
		return "", fmt.Errorf("%q is not http or https", raw)
	case u.Host == "" || u.Hostname() == "":
		return "", fmt.Errorf("%q names no host", raw)
	case u.User != nil:
		return "", fmt.Errorf("%q names a user: a key goes in pocketd login", raw)
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return "", fmt.Errorf("%q has a query or a fragment", raw)
	}
	return strings.TrimRight(raw, "/"), nil
}

// decodeString decodes a JSON string, and nothing else, into out.
func decodeString(dec *json.Decoder, out *string) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if len(raw) == 0 || raw[0] != '"' {
		return fmt.Errorf("%s is not text", raw)
	}
	return json.Unmarshal(raw, out)
}

// readObject reads one JSON object from dec, calling field for each of its
// keys, as written, in order; a key named twice is refused.
func readObject(dec *json.Decoder, where string, field func(key string) error) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('{') {
		return fmt.Errorf("%s is no object", where)
	}
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("%s: %w", where, err)
		}
		key, _ := tok.(string)
		if seen[key] {
			return fmt.Errorf("%s names %q twice", where, key)
		}
		seen[key] = true
		if err := field(key); err != nil {
			return err
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim('}') {
		return fmt.Errorf("%s does not end", where)
	}
	return nil
}

// readArray reads one JSON array from dec, calling item for each element.
func readArray(dec *json.Decoder, where string, item func() error) error {
	if tok, err := dec.Token(); err != nil || tok != json.Delim('[') {
		return fmt.Errorf("%s is no list", where)
	}
	for dec.More() {
		if err := item(); err != nil {
			return err
		}
	}
	if tok, err := dec.Token(); err != nil || tok != json.Delim(']') {
		return fmt.Errorf("%s does not end", where)
	}
	return nil
}

// Providers reads the folder's own providers.
func (f Folder) Providers(builtin func(id string) bool) ([]Provider, error) {
	if err := f.check(); err != nil {
		return nil, err
	}
	return LoadProviders(f.Dir, builtin)
}
