package router

import (
	"errors"
	"slices"
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

// An owner's model does not rescue a profile whose own model cannot run.
func TestChooseChecksTheProfileFirst(t *testing.T) {
	haiku := proto.ModelRef{Provider: "anthropic", ID: "claude-haiku-5-5"}
	if _, err := Choose(Request{Owner: haiku, Profile: profile("github-copilot/gpt-6-luna")}, available); err == nil {
		t.Error("an owner's model started a profile that cannot run")
	}
}

// A family is the model's maker, whoever serves it (ADR-0017).
func TestFamilyIsTheModelsMaker(t *testing.T) {
	for _, c := range []struct {
		provider, id, want string
	}{
		{"anthropic", "claude-opus-5-5", FamilyAnthropic},
		{"github-copilot", "claude-sonnet-5.5", FamilyAnthropic},
		{"openrouter", "anthropic/claude-haiku-5-5", FamilyAnthropic},
		{"openai", "gpt-6-astra", FamilyOpenAI},
		{"openai-codex", "gpt-6.1-sol", FamilyOpenAI},
		{"github-copilot", "gpt-6-luna", FamilyOpenAI},
		{"openai", "o3-mini", FamilyOpenAI},
		{"openai", "o4", FamilyOpenAI},
		{"openai", "codex-mini", FamilyOpenAI},
		{"openai", "gpt5", FamilyOpenAI},
		{"google", "gemini-3-pro", FamilyGoogle},
		{"github-copilot", "Gemini-3-Pro", FamilyGoogle},
		{"deepseek", "deepseek-v4", FamilyDeepSeek},
		{"xai", "grok-5", FamilyXAI},
		{"anthropic", "some-new-model", ""},
		{"openai-codex", "some-new-model", ""},
		{"mistral", "zai-glm-5-2", ""},
		// A namespace names the maker; a derivative of another's base
		// model is no known family (Codex review of #23, round 3).
		{"openrouter", "nvidia/llama-3.1-nemotron-70b-instruct", ""},
		{"openrouter", "nvidia/some-model", ""},
		{"openrouter", "meta-llama/llama-4-maverick", FamilyMeta},
		{"openrouter", "mistralai/ministral-14b-2512", FamilyMistral},
		{"openrouter", "mistralai/claude-x", ""},
		{"openrouter", "mistralai/mixtral-8x22b-instruct", FamilyMistral},
		{"openrouter", "meta/muse-spark-1.2", FamilyMeta},
		{"mistral", "mixtral-8x7b", FamilyMistral},
		{"openrouter", "openai/claude-mix", ""},
		{"nvidia", "llama-3.1-nemotron-70b-instruct", ""},
		{"mistral", "mistral-large-3", FamilyMistral},
		{"github-copilot", "some-new-model", ""},
		{"openrouter", "o30x", ""},
		{"", "", ""},
	} {
		if got := Family(proto.ModelRef{Provider: c.provider, ID: c.id}); got != c.want {
			t.Errorf("Family(%s/%s) = %q, want %q", c.provider, c.id, got, c.want)
		}
	}
}

// A cross-review runs the reviewer model of the author's family, the
// owner's policy, whatever the agent's own model is (ADR-0019).
func TestChooseTheReviewerModel(t *testing.T) {
	served := append(slices.Clone(available),
		proto.Model{ModelRef: proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}},
		proto.Model{ModelRef: proto.ModelRef{Provider: "anthropic", ID: "claude-sonnet-5-5"}})
	// The agent's own model is one pocketd cannot run: a review runs anyway.
	agent := profile("openai/gpt-6.2-sol")
	for name, tc := range map[string]struct {
		author proto.ModelRef
		want   proto.ModelRef
	}{
		"a Claude author":     {proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}, proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}},
		"a GPT author":        {proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}, proto.ModelRef{Provider: "anthropic", ID: "claude-sonnet-5-5"}},
		"a Claude on Copilot": {proto.ModelRef{Provider: "github-copilot", ID: "claude-sonnet-5.5"}, proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}},
		"another family":      {proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4"}, proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}},
	} {
		got, err := Choose(Request{Profile: agent, ReviewOf: tc.author}, served)
		if err != nil || got != (Choice{tc.want, RuleCrossReview}) {
			t.Errorf("%s: %+v, %v, want %v", name, got, err, tc.want)
		}
		if Family(got.Model) == Family(tc.author) {
			t.Errorf("%s: reviewed by its own family", name)
		}
	}
}

// No known family is no review, and a reviewer model no sign-in serves
// starts nothing (ADR-0019).
func TestChooseRefusesAReviewer(t *testing.T) {
	if _, err := Choose(Request{ReviewOf: proto.ModelRef{Provider: "nvidia", ID: "nemotron-ultra"}}, available); err == nil || !strings.Contains(err.Error(), "no model family") {
		t.Errorf("an author of no family: %v", err)
	}
	if _, err := Choose(Request{ReviewOf: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}}, available); err == nil || !strings.Contains(err.Error(), "not available") {
		t.Errorf("a reviewer model no sign-in serves: %v", err)
	}
	for family, reviewer := range reviewers {
		if Family(reviewer) == family {
			t.Errorf("the %s family is reviewed by its own family's %s", family, reviewer)
		}
	}
}

// A thread's worker runs the worker model its specialist's profile names,
// else the team's, as pocketd runs it; one pocketd cannot run is refused
// (ADR-0021).
func TestChooseTheWorkerModel(t *testing.T) {
	flash := proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}
	workers := append(slices.Clone(available), proto.Model{ModelRef: flash})
	got, err := Choose(Request{Worker: true, Profile: profile("anthropic/claude-opus-5-5")}, workers)
	if err != nil || got != (Choice{flash, RuleWorkerDefault}) {
		t.Fatalf("the default worker: %+v, %v", got, err)
	}
	if got, err := Choose(Request{Worker: true}, workers); err != nil || got != (Choice{flash, RuleWorkerDefault}) {
		t.Fatalf("no profile: %+v, %v", got, err)
	}
	named := profile("anthropic/claude-opus-5-5")
	named.Worker = proto.ModelRef{Provider: "openai", ID: "gpt-6-astra"}
	if got, err := Choose(Request{Worker: true, Profile: named}, workers); err != nil ||
		got != (Choice{proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}, RuleWorker}) {
		t.Fatalf("the profile's worker, under its alias: %+v, %v", got, err)
	}
	if _, err := Choose(Request{Worker: true, Profile: profile("anthropic/claude-opus-5-5")}, available); err == nil ||
		!strings.Contains(err.Error(), "pocketd login") {
		t.Fatalf("no worker model signed in: %v", err)
	}
}

// A worker-only model is chosen for nothing but a worker: not as the
// owner's model, nor as a profile's (ADR-0021).
func TestChooseRefusesAWorkerOnlyModel(t *testing.T) {
	mai := proto.ModelRef{Provider: "github-copilot", ID: "mai-code-1.1-flash"}
	models := append(slices.Clone(available), proto.Model{ModelRef: mai})
	if !WorkerOnly(mai) || WorkerOnly(proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}) {
		t.Fatal("WorkerOnly does not tell MAI-Code")
	}
	if _, err := Choose(Request{Owner: mai}, models); err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("the owner's MAI-Code: %v", err)
	}
	if _, err := Choose(Request{Profile: profile("github-copilot/mai-code-1.1-flash")}, models); err == nil || !strings.Contains(err.Error(), "worker") {
		t.Fatalf("a profile on MAI-Code: %v", err)
	}
	named := profile("anthropic/claude-opus-5-5")
	named.Worker = mai
	if got, err := Choose(Request{Worker: true, Profile: named}, models); err != nil || got != (Choice{mai, RuleWorker}) {
		t.Fatalf("MAI-Code as a worker: %+v, %v", got, err)
	}
}
