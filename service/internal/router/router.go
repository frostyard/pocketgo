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
	// RuleCrossReview is the reviewer model of a cross-review, by the
	// author's family (ADR-0019), as written or under an alias.
	RuleCrossReview Rule = "cross-review"
	// RuleWorker is the worker model a thread's profile names (ADR-0021).
	RuleWorker Rule = "worker"
	// RuleWorkerDefault is the team's worker model, for a profile that
	// names none (ADR-0021).
	RuleWorkerDefault Rule = "worker-default"
)

// DefaultWorker is the team's worker model: the DeepSeek API's cheapest,
// the owner's practice (ADR-0021).
var DefaultWorker = proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}

// reviewers are the reviewer models by the author's family: the owner's
// cross-review policy (ADR-0019). The owner names
// github-copilot/claude-sonnet-5.5 for a GPT author; pocketd runs the same
// maker's model on its own sign-in. A family not named here gets
// otherReviewer; no family gets no review.
var reviewers = map[string]proto.ModelRef{
	FamilyAnthropic: {Provider: "openai", ID: "gpt-6.1-sol"},
	FamilyOpenAI:    {Provider: "anthropic", ID: "claude-sonnet-5-5"},
}

// otherReviewer reviews an author of a known family the table does not
// name (ADR-0019).
var otherReviewer = proto.ModelRef{Provider: "openai", ID: "gpt-6.1-sol"}

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
	// ReviewOf is the model that wrote the change a cross-review reads,
	// zero for no review: the session then runs the reviewer model of its
	// family (ADR-0019), whatever the profile's own model is.
	ReviewOf proto.ModelRef
	// Worker is a thread's worker (ADR-0021): it runs the worker model of
	// Profile, the thread's specialist's, else the team's.
	Worker bool
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
	// A worker runs its worker model, which may be one only workers run
	// (ADR-0021).
	if request.Worker {
		return workerChoice(request.Profile, runs)
	}
	// A cross-review runs the reviewer model alone: its agent's own model
	// does not run in it (ADR-0019).
	if request.ReviewOf != (proto.ModelRef{}) {
		return reviewChoice(request.ReviewOf, runs)
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
	if got, ok := served(want, runs); ok {
		rule := RuleProfile
		if got != want {
			rule = RuleProfileAlias
		}
		return Choice{Model: got, Rule: rule}, nil
	}
	return Choice{}, fmt.Errorf("profile %s runs %s, which pocketd cannot run: no sign-in serves it (GET %s lists the models)",
		p.ID, want, proto.PathModels)
}

// served is want as pocketd runs it: as written, or under an alias.
func served(want proto.ModelRef, runs func(proto.ModelRef) bool) (proto.ModelRef, bool) {
	if runs(want) {
		return want, true
	}
	for _, provider := range aliases[want.Provider] {
		if alias := (proto.ModelRef{Provider: provider, ID: want.ID}); runs(alias) {
			return alias, true
		}
	}
	return proto.ModelRef{}, false
}

// workerChoice is the worker model of a thread whose specialist's profile
// is p (nil for none): the profile's, else the team's, as pocketd runs it
// (ADR-0021).
func workerChoice(p *profiles.Profile, runs func(proto.ModelRef) bool) (Choice, error) {
	want, rule := DefaultWorker, RuleWorkerDefault
	if p != nil && p.Worker != (proto.ModelRef{}) {
		want, rule = p.Worker, RuleWorker
	}
	got, ok := served(want, runs)
	if !ok {
		return Choice{}, fmt.Errorf("the worker model %s is not available: no sign-in serves it (pocketd models lists the worker models; the owner adds a key with pocketd login)", want)
	}
	return Choice{Model: got, Rule: rule}, nil
}

// ReviewerOf is the reviewer model of a change author wrote (ADR-0019):
// by author's family, never of that family. ok is false for an author of
// no known family: its change gets no review, and the owner decides.
func ReviewerOf(author proto.ModelRef) (proto.ModelRef, bool) {
	family := Family(author)
	if family == "" {
		return proto.ModelRef{}, false
	}
	reviewer, named := reviewers[family]
	if !named {
		reviewer = otherReviewer
	}
	if Family(reviewer) == family {
		return proto.ModelRef{}, false // the table never reviews a family by itself
	}
	return reviewer, true
}

// reviewChoice is the reviewer model of a change author wrote, as pocketd
// runs it.
func reviewChoice(author proto.ModelRef, runs func(proto.ModelRef) bool) (Choice, error) {
	reviewer, ok := ReviewerOf(author)
	if !ok {
		return Choice{}, fmt.Errorf("%s is of no model family pocketd knows, so no reviewer model is of another: the owner decides", author)
	}
	got, ok := served(reviewer, runs)
	if !ok {
		return Choice{}, fmt.Errorf("the reviewer model %s is not available: no sign-in serves it (GET %s lists the models)", reviewer, proto.PathModels)
	}
	return Choice{Model: got, Rule: RuleCrossReview}, nil
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
