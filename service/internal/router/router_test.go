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
	local := proto.ModelRef{Provider: "halogen", ID: "qwen"}
	workers := append(slices.Clone(available), proto.Model{ModelRef: flash})
	got, err := Choose(Request{Worker: true, Profile: profile("anthropic/claude-opus-5-5")}, workers)
	if err != nil || got != (Choice{flash, RuleWorkerDefault}) {
		t.Fatalf("the default worker: %+v, %v", got, err)
	}
	if got, err := Choose(Request{Worker: true}, workers); err != nil || got != (Choice{flash, RuleWorkerDefault}) {
		t.Fatalf("no profile: %+v, %v", got, err)
	}
	// The owner's list: its first model pocketd runs, in order
	// (ADR-0022).
	list := profiles.Workers{Models: []proto.ModelRef{local, {Provider: "openai", ID: "gpt-6-astra"}, flash}}
	if got, err := Choose(Request{Worker: true, Workers: list}, workers); err != nil ||
		got != (Choice{proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}, RuleWorkerTeam}) {
		t.Fatalf("the list's first model pocketd runs, under its alias: %+v, %v", got, err)
	}
	if got, err := Choose(Request{Worker: true, Workers: list}, append(slices.Clone(workers), proto.Model{ModelRef: local})); err != nil ||
		got != (Choice{local, RuleWorkerTeam}) {
		t.Fatalf("the list's first model: %+v, %v", got, err)
	}
	named := profile("anthropic/claude-opus-5-5")
	named.Worker = proto.ModelRef{Provider: "openai", ID: "gpt-6-astra"}
	if got, err := Choose(Request{Worker: true, Profile: named, Workers: profiles.Workers{Models: []proto.ModelRef{flash}}}, workers); err != nil ||
		got != (Choice{proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}, RuleWorker}) {
		t.Fatalf("the profile's worker, under its alias, over the list: %+v, %v", got, err)
	}
	// The model the supervisor asked for wins over the profile and the
	// list.
	if got, err := Choose(Request{Worker: true, Profile: named, Workers: list, WorkerAsked: flash}, workers); err != nil ||
		got != (Choice{flash, RuleWorkerAsked}) {
		t.Fatalf("the asked worker: %+v, %v", got, err)
	}
	// A model asked for, or the profile's, that pocketd cannot run
	// refuses: it never falls back to the list.
	if _, err := Choose(Request{Worker: true, Workers: list, WorkerAsked: local}, workers); err == nil || !strings.Contains(err.Error(), "the supervisor asked for") {
		t.Fatalf("an asked worker pocketd cannot run: %v", err)
	}
	gone := profile("anthropic/claude-opus-5-5")
	gone.Worker = local
	if _, err := Choose(Request{Worker: true, Profile: gone, Workers: list}, workers); err == nil || !strings.Contains(err.Error(), "worker model halogen/qwen") {
		t.Fatalf("a profile's worker pocketd cannot run: %v", err)
	}
	if _, err := Choose(Request{Worker: true, Profile: profile("anthropic/claude-opus-5-5")}, available); err == nil ||
		!strings.Contains(err.Error(), "pocketd login") || !strings.Contains(err.Error(), profiles.WorkersFile) {
		t.Fatalf("no worker model signed in: %v", err)
	}
}

// The supervisor names an entry of the owner's list, by provider or whole
// reference, for the owner (ADR-0022): nothing else.
func TestAskedWorker(t *testing.T) {
	flash := proto.ModelRef{Provider: "deepseek", ID: "deepseek-flash"}
	pro := proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4-pro"}
	astra := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6-astra"}
	list := profiles.Workers{Models: []proto.ModelRef{{Provider: "halogen", ID: "qwen"}, pro, flash, astra}}
	for name, want := range map[string]proto.ModelRef{
		"deepseek":                 pro,
		" deepseek ":               pro,
		"deepseek/deepseek-flash":  flash,
		"openai":                   astra,
		"openai/gpt-6-astra":       astra,
		"openai-codex/gpt-6-astra": astra,
	} {
		if got, err := AskedWorker(name, list); err != nil || got != want {
			t.Errorf("%q: %v %v, want %v", name, got, err, want)
		}
	}
	for _, name := range []string{"", "anthropic", "deepseek/deepseek-v9", "deepseek/", "Deepseek"} {
		if got, err := AskedWorker(name, list); err == nil {
			t.Errorf("%q resolved to %v", name, got)
		} else if !strings.Contains(err.Error(), list.String()) {
			t.Errorf("%q: the refusal does not list the entries: %v", name, err)
		}
	}
	// A provider as written wins over an alias of it, as the owner named
	// it (Codex review of #33).
	both := profiles.Workers{Models: []proto.ModelRef{{Provider: "openai", ID: "gpt-6-astra"}, astra}}
	for name, want := range map[string]proto.ModelRef{
		"openai-codex/gpt-6-astra": astra,
		"openai-codex":             astra,
		"openai/gpt-6-astra":       {Provider: "openai", ID: "gpt-6-astra"},
		"openai":                   {Provider: "openai", ID: "gpt-6-astra"},
	} {
		if got, err := AskedWorker(name, both); err != nil || got != want {
			t.Errorf("%q in a list of both: %v %v, want %v", name, got, err, want)
		}
	}
	// With no list, the default list's model.
	if got, err := AskedWorker("deepseek", profiles.Workers{}); err != nil || got != flash {
		t.Errorf("the default list: %v %v", got, err)
	}
}

// A model's window is its listed one; a model not listed has none.
func TestWindow(t *testing.T) {
	big := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	faux := proto.ModelRef{Provider: "faux", ID: "faux-1"}
	available := []proto.Model{{ModelRef: big, ContextWindow: 1_000_000}, {ModelRef: faux}}
	if got := Window(available, big); got != 1_000_000 {
		t.Errorf("listed: %d", got)
	}
	if got := Window(available, faux); got != 0 {
		t.Errorf("no window: %d", got)
	}
	if got := Window(available, proto.ModelRef{Provider: "openai", ID: "gpt-6"}); got != 0 {
		t.Errorf("not listed: %d", got)
	}
}

// The owner's change from the composer (ADR-0028): a model the service
// runs, by the owner's rule; a level the model takes; without one, the
// session's level when the new model takes it, else the model's default.
func TestChange(t *testing.T) {
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	sol := proto.ModelRef{Provider: "openai-codex", ID: "gpt-6.1-sol"}
	flash := proto.ModelRef{Provider: "deepseek", ID: "deepseek-v4-flash"}
	models := []proto.Model{
		{ModelRef: opus, ThinkingLevels: []string{"off", "low", "medium", "high", "xhigh"}},
		{ModelRef: sol, ThinkingLevels: []string{"off", "low", "medium", "high"}},
		{ModelRef: flash, ThinkingLevels: []string{"off"}},
	}
	for _, tc := range []struct {
		name            string
		model           *proto.ModelRef
		level, thinking string
		want            proto.ModelRef
		wantLevel       string
		refused         bool
	}{
		{name: "a model keeps a level it takes", model: &sol, level: "high", want: sol, wantLevel: "high"},
		{name: "a model that lacks the level runs at its default", model: &sol, level: "xhigh", want: sol},
		{name: "a model and a level", model: &flash, level: "high", thinking: "off", want: flash, wantLevel: "off"},
		{name: "a level alone keeps the model", level: "low", thinking: "xhigh", want: opus, wantLevel: "xhigh"},
		{name: "the default level stays", model: &sol, want: sol},
		{name: "a level the model does not take", model: &flash, thinking: "high", refused: true},
		{name: "a level alone the model does not take", thinking: "max", refused: true},
		{name: "a model the service does not run", model: &proto.ModelRef{Provider: "anthropic", ID: "claude-gone"}, refused: true},
	} {
		choice, level, err := Change(opus, tc.level, tc.model, tc.thinking, models)
		if tc.refused {
			if err == nil {
				t.Errorf("%s: chose %v at %q, want a refusal", tc.name, choice, level)
			}
			continue
		}
		if err != nil || choice.Model != tc.want || choice.Rule != RuleOwner || level != tc.wantLevel {
			t.Errorf("%s: chose %+v at %q (%v), want %v by the owner's rule at %q", tc.name, choice, level, err, tc.want, tc.wantLevel)
		}
	}
}

// Levels compare as PiG reads them: an empty level, the model's default,
// is off.
func TestSameLevel(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"", "", true}, {"", "off", true}, {"off", "", true}, {"high", "high", true},
		{"", "low", false}, {"high", "off", false}, {"low", "high", false},
	} {
		if got := SameLevel(tc.a, tc.b); got != tc.same {
			t.Errorf("SameLevel(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}
