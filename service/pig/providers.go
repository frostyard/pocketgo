package pig

import (
	"context"
	"slices"

	"github.com/MichaelKinsy/PiG/ai"

	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// noKey is the key sent to an owner's provider that takes none: PiG's
// adapters refuse to send a request without one, and such a server takes
// any (ADR-0023).
const noKey = "none"

// Builtin says whether id names one of PiG's own providers, which an
// owner's provider may not take (ADR-0023).
func Builtin(id string) bool {
	return slices.ContainsFunc(ai.BuiltinProviders(), func(p *ai.ModelsProvider) bool { return p != nil && p.ID == id })
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
		API:  ai.ProviderAPIMap{ai.APIOpenAICompletions: ai.OpenAICompletionsAPI(), ai.APIAnthropicMessages: ai.AnthropicMessagesAPI()},
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

// ownIDs are the ids of the owner's providers.
func ownIDs(own []profiles.Provider) map[string]profiles.Provider {
	ids := make(map[string]profiles.Provider, len(own))
	for _, p := range own {
		ids[p.ID] = p
	}
	return ids
}
