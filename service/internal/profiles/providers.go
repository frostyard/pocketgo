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

// Own is what the owner's providers file adds to PiG's catalog
// (ADR-0023): their own providers, and models of PiG's own providers that
// its catalog does not have yet.
type Own struct {
	Providers []Provider
	Added     []AddedModels
}

// AddedModels are models the owner adds to one of PiG's own providers.
type AddedModels struct {
	Provider string
	Models   []AddedModel
}

// AddedModel is a model of one of PiG's own providers that its catalog
// does not have yet: it runs as the catalog's model Like does, under its
// own id, with its own limits where they are set (0 keeps Like's).
type AddedModel struct {
	ID, Name, Like           string
	ContextWindow, MaxTokens int
}

var providerID = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// LoadProviders reads the owner's providers file in dir: nothing when
// there is no file. builtin says whether an id names one of PiG's own
// providers. A file that is no regular file of its own, or breaks a rule
// of ParseProviders, is an error: nothing of it is registered.
func LoadProviders(dir string, builtin func(id string) bool) (Own, error) {
	data, err := readProfile(filepath.Join(dir, ProvidersFile))
	if errors.Is(err, fs.ErrNotExist) {
		return Own{}, nil
	}
	if err != nil {
		return Own{}, fmt.Errorf("%s: %w", ProvidersFile, err)
	}
	own, err := ParseProviders(data, builtin)
	if err != nil {
		return Own{}, fmt.Errorf("%s: %w", ProvidersFile, err)
	}
	return own, nil
}

// ParseProviders checks the owner's providers (ADR-0023). Every object's
// keys are read as written, since a decoder matches a key in any case and
// keeps the last of one named twice: another case, a key named twice or
// an unknown key is refused.
//
// The file holds "providers", the owner's own, and "builtinModels",
// models added to PiG's own providers; at least one of them.
func ParseProviders(data []byte, builtin func(id string) bool) (Own, error) {
	if len(data) > maxProfileSize {
		return Own{}, fmt.Errorf("the file is larger than %d bytes", maxProfileSize)
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	var own Own
	var seenProviders, seenAdded bool
	err := readObject(dec, "the file", func(key string) error {
		switch key {
		case "providers":
			seenProviders = true
			return readArray(dec, "providers", func() error {
				p, err := readProvider(dec, len(own.Providers)+1)
				if err != nil {
					return err
				}
				own.Providers = append(own.Providers, p)
				return nil
			})
		case "builtinModels":
			seenAdded = true
			return readArray(dec, "builtinModels", func() error {
				a, err := readAdded(dec, len(own.Added)+1)
				if err != nil {
					return err
				}
				own.Added = append(own.Added, a)
				return nil
			})
		}
		return fmt.Errorf("the file has the field %q: it takes providers and builtinModels", key)
	})
	if err != nil {
		return Own{}, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return Own{}, errors.New("text follows the file's object")
	}
	switch {
	case !seenProviders && !seenAdded:
		return Own{}, errors.New(`it names no providers and no builtinModels: {"providers": [{…}], "builtinModels": [{…}]}`)
	case seenProviders && (len(own.Providers) == 0 || len(own.Providers) > MaxProviders):
		return Own{}, fmt.Errorf("it names %d providers, not 1 to %d", len(own.Providers), MaxProviders)
	case seenAdded && (len(own.Added) == 0 || len(own.Added) > MaxProviders):
		return Own{}, fmt.Errorf("its builtinModels name %d providers, not 1 to %d", len(own.Added), MaxProviders)
	}
	ids := map[string]bool{}
	for _, p := range own.Providers {
		if builtin != nil && builtin(p.ID) {
			return Own{}, fmt.Errorf("provider %q is one of pocketd's own: give yours another id, or add its models under builtinModels", p.ID)
		}
		if ids[p.ID] {
			return Own{}, fmt.Errorf("provider %q is named twice", p.ID)
		}
		ids[p.ID] = true
	}
	added := map[string]bool{}
	for _, a := range own.Added {
		if builtin != nil && !builtin(a.Provider) {
			return Own{}, fmt.Errorf("builtinModels: %q is none of pocketd's own providers: an owner's provider names its models under providers", a.Provider)
		}
		if added[a.Provider] {
			return Own{}, fmt.Errorf("builtinModels: provider %q is named twice", a.Provider)
		}
		added[a.Provider] = true
	}
	return own, nil
}

// readAdded reads and checks entry n of builtinModels.
func readAdded(dec *json.Decoder, n int) (AddedModels, error) {
	var a AddedModels
	var hasModels bool
	where := fmt.Sprintf("builtinModels %d", n)
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "provider":
			return decodeString(dec, &a.Provider)
		case "models":
			hasModels = true
			return readArray(dec, where+"'s models", func() error {
				m, err := readAddedModel(dec, fmt.Sprintf("%s's model %d", where, len(a.Models)+1))
				if err != nil {
					return err
				}
				a.Models = append(a.Models, m)
				return nil
			})
		}
		return fmt.Errorf("%s has the field %q: it takes provider and models", where, key)
	})
	if err != nil {
		return AddedModels{}, err
	}
	if !providerID.MatchString(a.Provider) {
		return AddedModels{}, fmt.Errorf("%s: the provider %q is no provider id", where, a.Provider)
	}
	where = fmt.Sprintf("builtinModels for %q", a.Provider)
	if !hasModels || len(a.Models) == 0 || len(a.Models) > MaxProviderModels {
		return AddedModels{}, fmt.Errorf("%s name %d models, not 1 to %d", where, len(a.Models), MaxProviderModels)
	}
	ids := map[string]bool{}
	for _, m := range a.Models {
		if ids[m.ID] {
			return AddedModels{}, fmt.Errorf("%s: the model %q is named twice", where, m.ID)
		}
		ids[m.ID] = true
	}
	return a, nil
}

// readAddedModel reads and checks one model added to one of PiG's own
// providers.
func readAddedModel(dec *json.Decoder, where string) (AddedModel, error) {
	var m AddedModel
	err := readObject(dec, where, func(key string) error {
		switch key {
		case "id":
			return decodeString(dec, &m.ID)
		case "name":
			return decodeString(dec, &m.Name)
		case "like":
			return decodeString(dec, &m.Like)
		case "contextWindow":
			return decodeInt(dec, &m.ContextWindow)
		case "maxTokens":
			return decodeInt(dec, &m.MaxTokens)
		}
		return fmt.Errorf("%s has the field %q: it takes id, name, like, contextWindow and maxTokens", where, key)
	})
	if err != nil {
		return AddedModel{}, err
	}
	if err := checkModelID(m.ID); err != nil {
		return AddedModel{}, fmt.Errorf("%s: the id %w", where, err)
	}
	if err := checkModelID(m.Like); err != nil {
		return AddedModel{}, fmt.Errorf("%s: like, the catalog's model it runs as, %w", where, err)
	}
	if m.Like == m.ID {
		return AddedModel{}, fmt.Errorf("%s: it is like itself", where)
	}
	if err := checkName(m.Name, maxProviderName); err != nil {
		return AddedModel{}, fmt.Errorf("%s: the name %w", where, err)
	}
	if m.Name == "" {
		m.Name = m.ID
	}
	if m.ContextWindow < 0 || m.ContextWindow > maxContextWindow {
		return AddedModel{}, fmt.Errorf("%s: the contextWindow %d is not 1 to %d", where, m.ContextWindow, maxContextWindow)
	}
	if m.MaxTokens < 0 || (m.ContextWindow > 0 && m.MaxTokens > m.ContextWindow) {
		return AddedModel{}, fmt.Errorf("%s: the maxTokens %d is not 1 to the contextWindow", where, m.MaxTokens)
	}
	return m, nil
}

// checkModelID refuses a model id that is empty, too long, or holds a
// space or a control character.
func checkModelID(id string) error {
	if id == "" || len(id) > maxModelID || strings.ContainsFunc(id, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) || !utf8.ValidString(id) {
		return fmt.Errorf("%q is empty, longer than %d bytes, or holds a space", id, maxModelID)
	}
	return nil
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
			return decodeBool(dec, &p.Key)
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
	if err := checkName(p.Name, maxProviderName); err != nil {
		return Provider{}, fmt.Errorf("%s: the name %w", where, err)
	}
	if p.Name == "" {
		p.Name = p.ID
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
			return decodeInt(dec, &m.ContextWindow)
		case "maxTokens":
			return decodeInt(dec, &m.MaxTokens)
		case "reasoning":
			return decodeBool(dec, &m.Reasoning)
		case "images":
			return decodeBool(dec, &m.Images)
		}
		return fmt.Errorf("%s has the field %q: it takes id, name, contextWindow, maxTokens, reasoning and images", where, key)
	})
	if err != nil {
		return ProviderModel{}, err
	}
	if err := checkModelID(m.ID); err != nil {
		return ProviderModel{}, fmt.Errorf("%s: the id %w", where, err)
	}
	// The limit holds for a name the owner wrote: one left out is the id,
	// which may be longer (Codex review of #34).
	if err := checkName(m.Name, maxProviderName); err != nil {
		return ProviderModel{}, fmt.Errorf("%s: the name %w", where, err)
	}
	if m.Name == "" {
		m.Name = m.ID
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
// A refusal never repeats the URL, nor url.Parse's error, which quotes it:
// it may hold a password or a token, and pocketd logs the refusal (Codex
// review of #34).
func checkBaseURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	switch {
	case raw == "":
		return "", errors.New("is missing")
	case err != nil:
		return "", errors.New("is no URL")
	case u.Scheme != "http" && u.Scheme != "https":
		return "", errors.New("is not http or https")
	case u.Host == "" || u.Hostname() == "":
		return "", errors.New("names no host")
	case u.User != nil:
		return "", errors.New("names a user or a password: a key goes in pocketd login")
	case u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || strings.Contains(raw, "#"):
		return "", errors.New("has a query or a fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

// decodeBool decodes a JSON true or false, and nothing else, into out: a
// decoder takes null into a bool and leaves it false (Codex review of
// #34).
func decodeBool(dec *json.Decoder, out *bool) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	switch string(raw) {
	case "true":
		*out = true
	case "false":
		*out = false
	default:
		return fmt.Errorf("%s is not true or false", raw)
	}
	return nil
}

// decodeInt decodes a JSON whole number, and nothing else, into out: a
// decoder takes null into a number and leaves it 0.
func decodeInt(dec *json.Decoder, out *int) error {
	var raw json.RawMessage
	if err := dec.Decode(&raw); err != nil {
		return err
	}
	if string(raw) == "null" {
		return errors.New("null is no number")
	}
	return json.Unmarshal(raw, out)
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

// Providers reads the folder's providers file.
func (f Folder) Providers(builtin func(id string) bool) (Own, error) {
	if err := f.check(); err != nil {
		return Own{}, err
	}
	return LoadProviders(f.Dir, builtin)
}
