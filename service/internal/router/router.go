// Package router chooses every model pocketd runs (ADR-0013). It is one
// seam, in code: a choice names the rule that made it, and pocketd records
// both. The rules today are the owner's choice and the profile's model;
// later rules (run records, the owner's allowed models) go here too.
package router

import (
	"errors"
	"fmt"
	"strings"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

// Rule names the rule that chose a model.
type Rule string

const (
	// RuleOwner is a model the owner named.
	RuleOwner Rule = "owner"
	// RuleProfile is the profile's model, as written.
	RuleProfile Rule = "profile"
	// RuleProfileAlias is the profile's model id under a provider that
	// serves its provider's models on a subscription (aliases).
	RuleProfileAlias Rule = "profile-alias"
)

// aliases are the providers that serve a provider's models on a
// subscription: the profiles name ChatGPT's models as openai/…, which
// pocketd runs as openai-codex/… (ADR-0005, ADR-0013).
var aliases = map[string][]string{"openai": {"openai-codex"}}

// Request is what a model is chosen for.
type Request struct {
	// Owner is the model the owner named; zero for none.
	Owner proto.ModelRef
	// Profile is the agent's profile; nil for none.
	Profile *profiles.Profile
}

// Choice is a chosen model and the rule that chose it.
type Choice struct {
	Model proto.ModelRef
	Rule  Rule
}

// ErrNoModel is a request that names no model and no profile.
var ErrNoModel = errors.New("name a model, or a profile whose model to run")

// Choose picks the model for request among available, the models pocketd
// can run now.
func Choose(request Request, available []proto.Model) (Choice, error) {
	runs := func(ref proto.ModelRef) bool {
		for _, m := range available {
			if m.ModelRef == ref {
				return true
			}
		}
		return false
	}
	// A profile must be able to start on its own model, whichever model
	// the owner names: GET /v1/profiles lists one that cannot with a
	// problem, and such a profile starts nothing (protocol rule 7b).
	var fromProfile Choice
	if request.Profile != nil {
		var err error
		if fromProfile, err = profileChoice(request.Profile, runs); err != nil {
			return Choice{}, err
		}
	}
	if request.Owner != (proto.ModelRef{}) {
		if !runs(request.Owner) {
			return Choice{}, fmt.Errorf("model %s is not available; GET %s lists the models", request.Owner, proto.PathModels)
		}
		return Choice{Model: request.Owner, Rule: RuleOwner}, nil
	}
	if request.Profile == nil {
		return Choice{}, ErrNoModel
	}
	return fromProfile, nil
}

// profileChoice is the profile's model, as written or under an alias.
func profileChoice(p *profiles.Profile, runs func(proto.ModelRef) bool) (Choice, error) {
	want := p.Model
	if runs(want) {
		return Choice{Model: want, Rule: RuleProfile}, nil
	}
	for _, provider := range aliases[want.Provider] {
		if alias := (proto.ModelRef{Provider: provider, ID: want.ID}); runs(alias) {
			return Choice{Model: alias, Rule: RuleProfileAlias}, nil
		}
	}
	return Choice{}, fmt.Errorf("profile %s runs %s, which pocketd cannot run: no sign-in serves it (GET %s lists the models)",
		p.ID, want, proto.PathModels)
}

// Model families (ADR-0017): a family is a model's maker, not the provider
// that serves it, so a cross-review by another family reads the work
// with another maker's training.
const (
	FamilyAnthropic = "anthropic"
	FamilyOpenAI    = "openai"
	FamilyGoogle    = "google"
	FamilyDeepSeek  = "deepseek"
	FamilyXAI       = "xai"
	FamilyMeta      = "meta"
	FamilyMistral   = "mistral"
	FamilyAlibaba   = "alibaba"
)

// familyPrefixes are the model id prefixes of each family, checked in
// order: a model's id names its maker whoever serves it.
var familyPrefixes = []struct{ prefix, family string }{
	{"claude", FamilyAnthropic},
	{"gpt", FamilyOpenAI}, {"chatgpt", FamilyOpenAI}, {"codex", FamilyOpenAI},
	{"o1", FamilyOpenAI}, {"o3", FamilyOpenAI}, {"o4", FamilyOpenAI},
	{"gemini", FamilyGoogle}, {"gemma", FamilyGoogle},
	{"deepseek", FamilyDeepSeek},
	{"grok", FamilyXAI},
	{"llama", FamilyMeta},
	{"mistral", FamilyMistral}, {"ministral", FamilyMistral}, {"mixtral", FamilyMistral}, {"codestral", FamilyMistral}, {"devstral", FamilyMistral}, {"magistral", FamilyMistral},
	{"pixtral", FamilyMistral},
	{"qwen", FamilyAlibaba},
}

// Family is m's family, "" when code cannot tell it: by the model id's
// prefix (as "claude-opus-5-5", or "anthropic/claude-…" under a router).
// The provider tells nothing: a provider may serve another maker's
// models, as mistral serves zai-glm-5-2.
func Family(m proto.ModelRef) string {
	id := strings.ToLower(strings.TrimSpace(m.ID))
	// A derivative names its base model: its maker is another.
	for _, d := range derivatives {
		if strings.Contains(id, d) {
			return ""
		}
	}
	if i := strings.LastIndex(id, "/"); i >= 0 {
		// A namespace, as nvidia/llama-…, names the maker: one this rule
		// does not know tells nothing, whatever the id after it says.
		family, ok := namespaces[id[:i]]
		if !ok {
			return ""
		}
		// The rest may name no maker, as meta/muse-spark; one that names
		// another contradicts the namespace.
		if rest := familyOf(id[i+1:]); rest != "" && rest != family {
			return ""
		}
		return family
	}
	return familyOf(id)
}

// namespaces are the makers' namespaces in a router's model ids.
var namespaces = map[string]string{
	"anthropic": FamilyAnthropic, "openai": FamilyOpenAI, "google": FamilyGoogle, "deepseek": FamilyDeepSeek, "deepseek-ai": FamilyDeepSeek,
	"x-ai": FamilyXAI, "xai": FamilyXAI, "meta": FamilyMeta, "meta-llama": FamilyMeta, "mistralai": FamilyMistral, "qwen": FamilyAlibaba,
}

// derivatives are names of models another maker made from a base model.
var derivatives = []string{"nemotron"}

// familyOf is the family an id's prefix names.
func familyOf(id string) string {
	for _, f := range familyPrefixes {
		// A two-letter prefix, as o3, must end at a word's end: o3-mini,
		// never o30x.
		short := len(f.prefix) <= 2
		if strings.HasPrefix(id, f.prefix) && (!short || len(id) == len(f.prefix) || !isAlnum(id[len(f.prefix)])) {
			return f.family
		}
	}
	return ""
}

func isAlnum(b byte) bool {
	return b >= 'a' && b <= 'z' || b >= '0' && b <= '9'
}
