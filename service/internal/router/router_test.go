package router

import (
	"errors"
	"strings"
	"testing"

	"github.com/frostyard/pocketgo/proto"
	"github.com/frostyard/pocketgo/service/internal/profiles"
)

var available = []proto.Model{
	{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}},
	{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-haiku-5-5"}},
	{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}},
}

func profile(model string) *profiles.Profile {
	provider, id, _ := strings.Cut(model, "/")
	return &profiles.Profile{ID: "vera", Model: proto.ModelRef{Provider: provider, ID: id}}
}

func TestChoose(t *testing.T) {
	haiku := proto.ModelRef{Provider: "anthropic", ID: "claude-haiku-5-5"}
	for name, tc := range map[string]struct {
		request Request
		want    Choice
	}{
		"the profile's model": {Request{Profile: profile("anthropic/claude-opus-5-5")},
			Choice{proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, RuleProfile}},
		"openai served as openai-codex": {Request{Profile: profile("openai/gpt-6-astra")},
			Choice{proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}, RuleProfileAlias}},
		"the owner's model wins": {Request{Owner: haiku, Profile: profile("anthropic/claude-opus-5-5")},
			Choice{haiku, RuleOwner}},
		"the owner's model alone": {Request{Owner: haiku}, Choice{haiku, RuleOwner}},
	} {
		got, err := Choose(tc.request, available)
		if err != nil || got != tc.want {
			t.Errorf("%s: %+v, %v, want %+v", name, got, err, tc.want)
		}
	}
}

func TestChooseRefuses(t *testing.T) {
	if _, err := Choose(Request{}, available); !errors.Is(err, ErrNoModel) {
		t.Errorf("no model and no profile: %v", err)
	}
	// No sign-in serves Copilot: the error names the profile and its model.
	if _, err := Choose(Request{Profile: profile("github-copilot/gpt-6-luna")}, available); err == nil ||
		!strings.Contains(err.Error(), "vera") || !strings.Contains(err.Error(), "github-copilot/gpt-6-luna") {
		t.Errorf("an unserved profile model: %v", err)
	}
	// An alias applies only to the same model id.
	if _, err := Choose(Request{Profile: profile("openai/gpt-7")}, available); err == nil {
		t.Error("an aliased provider without the model was accepted")
	}
	if _, err := Choose(Request{Owner: proto.ModelRef{Provider: "openai", ID: "gpt-6-astra"}}, available); err == nil {
		t.Error("the owner's model is taken as written, and openai/gpt-6-astra is not served")
	}
}
