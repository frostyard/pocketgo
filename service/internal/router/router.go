// Package router chooses every model pocketd runs (ADR-0013). It is one
// seam, in code: a choice names the rule that made it, and pocketd records
// both. The rules today are the owner's choice and the profile's model;
// later rules (run records, the owner's allowed models) go here too.
package router

import (
	"errors"
	"fmt"

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
