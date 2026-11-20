package pig

import (
	"context"
	"errors"
	"fmt"
	"io"
	"maps"
	"net/http"
	"slices"
	"sync/atomic"
	"time"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// noKey is the key sent to an owner's provider that takes none: PiG's
// adapters send no request without a key, so a placeholder goes, which a
// server that takes no key ignores (ADR-0023; Codex review of #34).
const noKey = "none"

// Builtin says whether id names one of PiG's own providers, which an
// owner's provider may not take (ADR-0023). It reads the ids alone: the
// providers themselves build the whole catalog (Codex review of #34).
func Builtin(id string) bool {
	return slices.Contains(ai.ListProviders(), id)
}

// keptCredentials are the stored sign-ins as PiG reads them, with no value
// resolved: PiG reads a stored key that starts with ! as a command, and
// one that names $VAR as the service's environment, whatever the auth
// context (Codex review of #34). An API key is used only as stored, and
// only if it is a literal one (literalKey); an owner's provider that takes
// no key is never read. A subscription's sign-in reads as PiG reads it.
type keptCredentials struct {
	*ai.AuthStorage
	own map[string]profiles.Provider
}

func (c keptCredentials) Read(ctx context.Context, provider string) (*ai.Credential, error) {
	if p, ok := c.own[provider]; ok && !p.Key {
		return nil, nil
	}
	// One read, and the snapshot it checked is the one returned: PiG's
	// own read resolves nothing but an API key, so a subscription's
	// sign-in needs no second read, which could find another entry
	// (Codex review of #34, round 2).
	raw, ok, err := c.GetRaw(provider)
	if err != nil || !ok {
		return nil, err
	}
	if raw.Type == ai.CredentialAPIKey && literalKey(raw.Key) != nil {
		return nil, nil
	}
	return &raw, nil
}

// ownTransport carries the owner's providers' requests; nil is
// directTransport. Tests set it.
var ownTransport http.RoundTripper

// directTransport is Go's default transport with no proxy, whatever
// HTTP_PROXY says: a provider on the owner's network gets the conversation
// and the key itself, never a proxy of the service's environment (Codex
// review of #34, round 3). One for every request, so connections are
// kept.
var directTransport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.Proxy = nil
	t.ResponseHeaderTimeout = ownIdle
	return t
}()

// ownIdle bounds how long a provider may send nothing, as PiG's own
// transport does: its headers, and each read of its stream, never the
// whole of a stream that keeps sending (Codex review of #34, round 4).
var ownIdle = time.Duration(ai.DefaultHTTPIdleTimeoutMs) * time.Millisecond

// idleTransport ends a response whose body sends nothing for idle.
type idleTransport struct {
	next http.RoundTripper
	idle time.Duration
}

func (t idleTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	resp, err := t.next.RoundTrip(r)
	if err != nil || resp.Body == nil {
		return resp, err
	}
	resp.Body = newIdleBody(resp.Body, t.idle)
	return resp, nil
}

// idleTimer is the timer of an idleBody: time.AfterFunc's, which tests
// replace through newIdleTimer, so they wait on no clock.
type idleTimer interface {
	Reset(time.Duration) bool
	Stop() bool
}

var newIdleTimer = func(d time.Duration, f func()) idleTimer { return time.AfterFunc(d, f) }

// idleBody closes body once it has sent nothing for idle, and says why.
type idleBody struct {
	body    io.ReadCloser
	idle    time.Duration
	timer   idleTimer
	stalled atomic.Bool
}

func newIdleBody(body io.ReadCloser, idle time.Duration) *idleBody {
	b := &idleBody{body: body, idle: idle}
	b.timer = newIdleTimer(idle, func() {
		b.stalled.Store(true)
		_ = body.Close()
	})
	return b
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.body.Read(p)
	if b.stalled.Load() {
		return n, fmt.Errorf("the provider sent nothing for %s", b.idle)
	}
	if n > 0 {
		b.timer.Reset(b.idle)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.timer.Stop()
	return b.body.Close()
}

// ownClient is the HTTP client of the owner's providers' requests, on
// directTransport. It follows a redirect only to the origin it came from:
// a key in a header that Go keeps across hosts, as Anthropic's X-Api-Key,
// never reaches a host providers.json does not name (Codex review of #34,
// round 2).
func ownClient() *http.Client {
	var transport http.RoundTripper = directTransport
	if ownTransport != nil {
		transport = ownTransport
	}
	return &http.Client{Transport: idleTransport{next: transport, idle: ownIdle}, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if first := via[0].URL; req.URL.Scheme != first.Scheme || req.URL.Host != first.Host {
			return http.ErrUseLastResponse
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}}
}

// onOwnClient is streams with every request on ownClient.
func onOwnClient(streams *ai.ProviderStreams) *ai.ProviderStreams {
	on := *streams
	wrap := func(stream ai.ModelsStreamFunction) ai.ModelsStreamFunction {
		if stream == nil {
			return nil
		}
		return func(ctx context.Context, model *ai.Model, transcript ai.TranscriptContext, options ai.StreamOptions) (*ai.AssistantMessageEventStream, error) {
			options.Fetch = ownClient()
			return stream(ctx, model, transcript, options)
		}
	}
	on.Stream, on.StreamSimple = wrap(streams.Stream), wrap(streams.StreamSimple)
	return &on
}

// ownProvider is the owner's provider p as PiG runs it (ADR-0023): its
// models, each with p's base URL and API, on PiG's own adapters for that
// API, and its key from the stored sign-ins, or none.
func ownProvider(p profiles.Provider) *ai.ModelsProvider {
	api := ai.API(p.API)
	models := make([]ai.AnyModel, 0, len(p.Models))
	for _, m := range p.Models {
		input := []ai.ModelInputModality{ai.ModelInputText}
		if m.Images {
			input = append(input, ai.ModelInputImage)
		}
		var maxThinking ai.ThinkingLevel
		if m.Reasoning {
			maxThinking = ai.ThinkingLevelHigh
		}
		// The base URL goes on each model: the provider's is metadata
		// alone on the path a request takes.
		models = append(models, &ai.Model{
			Type: ai.ModelTypeChat, ID: m.ID, DisplayName: m.Name, Input: input,
			Capabilities: ai.ModelCapabilities{MaxThinking: maxThinking, SupportsImages: m.Images, SupportsToolUse: true,
				ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxTokens},
			ProviderMeta: ai.ProviderMetadata{ProviderID: p.ID, API: api, BaseURL: p.BaseURL, Reasoning: m.Reasoning},
		})
	}
	name := p.Name
	return ai.CreateProvider(ai.CreateProviderOptions{
		ID: p.ID, Name: &name, BaseURL: p.BaseURL, Models: models,
		Auth: ai.ProviderAuth{APIKey: &ai.APIKeyAuth{Name: p.Name + " API key", Resolve: ownKey(p.Key)}},
		API: ai.ProviderAPIMap{ai.APIOpenAICompletions: onOwnClient(ai.OpenAICompletionsAPI()),
			ai.APIAnthropicMessages: onOwnClient(ai.AnthropicMessagesAPI())},
	})
}

// ownKey resolves an owner's provider's key: the stored one of a provider
// that takes a key, and nothing until it is stored, so its models are not
// listed; noKey for one that takes none, whatever is stored.
func ownKey(keyed bool) func(context.Context, ai.APIKeyAuthInput) (*ai.AuthResult, error) {
	return func(ctx context.Context, in ai.APIKeyAuthInput) (*ai.AuthResult, error) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if !keyed {
			return &ai.AuthResult{Auth: ai.ModelAuth{APIKey: noKey}, Source: "no key"}, nil
		}
		if in.Credential != nil && in.Credential.Type == ai.CredentialAPIKey && in.Credential.Key != "" {
			return &ai.AuthResult{Auth: ai.ModelAuth{APIKey: in.Credential.Key}, Source: "stored key"}, nil
		}
		return nil, nil
	}
}

// addModels adds the owner's models to PiG's own providers in models
// (ADR-0023). Each runs as the catalog's model it is like, with every
// exported field of it, under its own id, name and limits; its catalog
// record is not copied, so the model is written as itself. A model the
// catalog has already, or one like a model it does not have, is not
// added: notes say why.
func addModels(models *ai.Models, added []profiles.AddedModels) (notes []string) {
	for _, a := range added {
		orig := models.GetProvider(a.Provider)
		if orig == nil || orig.GetModels == nil {
			notes = append(notes, fmt.Sprintf("%s is not among pocketd's providers: its models are not added", a.Provider))
			continue
		}
		base, err := orig.GetModels()
		if err != nil {
			notes = append(notes, fmt.Sprintf("%s's models cannot be read: %v", a.Provider, err))
			continue
		}
		// An id of any model type the catalog has stays the catalog's: an
		// image model never gets a chat route (Codex review of #34, round
		// 3). A model is like a chat model alone.
		taken := map[string]bool{}
		for _, b := range base {
			taken[b.ID] = true
		}
		if orig.GetAllModels != nil {
			all, err := orig.GetAllModels()
			if err != nil {
				notes = append(notes, fmt.Sprintf("%s's models cannot be read: %v", a.Provider, err))
				continue
			}
			for _, b := range all {
				taken[b.ModelID()] = true
			}
		}
		var extra []*ai.Model
		for n, m := range a.Models {
			// A note names a model the catalog has, or the entry by its
			// place, never a value the catalog did not match: it may be a
			// secret in the wrong place, and serve logs the note (Codex
			// review of #34, round 4).
			if taken[m.ID] {
				notes = append(notes, fmt.Sprintf("%s/%s is in PiG's catalog now: the catalog's runs, and its entry can go", a.Provider, m.ID))
				continue
			}
			i := slices.IndexFunc(base, func(b *ai.Model) bool { return b.ID == m.Like })
			if i < 0 {
				notes = append(notes, fmt.Sprintf("%s's added model %d is like a model PiG's catalog does not have: it is not added", a.Provider, n+1))
				continue
			}
			made := likeModel(base[i], m)
			if made.Capabilities.MaxOutputTokens > made.Capabilities.ContextWindow {
				notes = append(notes, fmt.Sprintf("%s's added model %d would write more than its context holds: it is not added", a.Provider, n+1))
				continue
			}
			extra = append(extra, made)
		}
		if len(extra) > 0 {
			models.SetProvider(withModels(orig, extra))
		}
	}
	return notes
}

// likeModel is the model m of the owner, made from the catalog's model
// like by its exported fields alone.
func likeModel(like *ai.Model, m profiles.AddedModel) *ai.Model {
	made := &ai.Model{
		Type: like.Type, ID: m.ID, DisplayName: m.Name, Provider: like.Provider, Capabilities: like.Capabilities,
		ProviderMeta: like.ProviderMeta, Input: slices.Clone(like.Input), ThinkingLevelMap: maps.Clone(like.ThinkingLevelMap),
		SamplingParams: maps.Clone(like.SamplingParams), SamplingParamsByThinkingLevel: like.SamplingParamsByThinkingLevel,
		PromptCache: like.PromptCache, InputLimits: like.InputLimits,
	}
	if m.ContextWindow > 0 {
		made.Capabilities.ContextWindow = m.ContextWindow
	}
	if m.MaxTokens > 0 {
		made.Capabilities.MaxOutputTokens = m.MaxTokens
	}
	return made
}

// withModels is provider orig with extra models after its own: the same
// sign-in, adapters and filters.
func withModels(orig *ai.ModelsProvider, extra []*ai.Model) *ai.ModelsProvider {
	p := *orig
	getModels := orig.GetModels
	p.GetModels = func() ([]*ai.Model, error) {
		base, err := getModels()
		if err != nil {
			return nil, err
		}
		return append(slices.Clone(base), extra...), nil
	}
	if getAll := orig.GetAllModels; getAll != nil {
		p.GetAllModels = func() ([]ai.AnyModel, error) {
			base, err := getAll()
			if err != nil {
				return nil, err
			}
			out := slices.Clone(base)
			for _, m := range extra {
				out = append(out, m)
			}
			return out, nil
		}
	}
	return &p
}

// CatalogDate is the day PiG's built-in catalog was generated, "" when it
// records none: the owner adds newer models under builtinModels
// (ADR-0023).
func CatalogDate() string {
	at := ai.GetBuiltinModelDataGeneratedAt()
	if at == nil {
		return ""
	}
	return time.UnixMilli(int64(*at)).UTC().Format(time.DateOnly)
}

// ownIDs are the ids of the owner's providers.
func ownIDs(own []profiles.Provider) map[string]profiles.Provider {
	ids := make(map[string]profiles.Provider, len(own))
	for _, p := range own {
		ids[p.ID] = p
	}
	return ids
}
