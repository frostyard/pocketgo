package state

import (
	"fmt"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/frostyard/pocketgo/proto"
)

func TestParseServer(t *testing.T) {
	for _, tc := range []struct {
		in, want, err string
	}{
		{in: "https://pocketd.example.ts.net:8443", want: "https://pocketd.example.ts.net:8443"},
		{in: "  HTTP://Example.ORG/  ", want: "http://example.org"},
		{in: "http://[::1]:8080", want: "http://[::1]:8080"},
		{in: "", err: "enter the service URL"},
		{in: "example.org:8080", err: "scheme"},
		{in: "pocketd.example.org", err: "scheme"},
		{in: "ftp://example.org", err: `scheme is "ftp"`},
		{in: "https://", err: "no host"},
		{in: "https://user:pass@example.org", err: "user name"},
		{in: "https://example.org/v1", err: "without a path"},
		{in: "https://example.org/?x=1", err: "without a path"},
	} {
		got, err := ParseServer(tc.in)
		switch {
		case tc.err != "" && (err == nil || !strings.Contains(err.Error(), tc.err)):
			t.Errorf("ParseServer(%q) = %q, %v; want an error with %q", tc.in, got, err, tc.err)
		case tc.err == "" && (err != nil || got != tc.want):
			t.Errorf("ParseServer(%q) = %q, %v; want %q", tc.in, got, err, tc.want)
		}
	}
}

func TestNeedsHTTPWarning(t *testing.T) {
	for _, tc := range []struct {
		server string
		warn   bool
	}{
		{"https://example.org", false},
		{"https://192.0.2.1", false},
		{"http://localhost:8080", false},
		{"http://127.0.0.1:8080", false},
		{"http://127.9.9.9", false},
		{"http://[::1]:8080", false},
		{"http://[::ffff:127.0.0.1]", false},
		{"http://100.64.0.1:8080", true},
		{"http://100.127.255.254", true},
		{"http://[fd7a:115c:a1e0::1]", true},
		{"http://pocketd.tail1234.ts.net:8080", false},
		{"http://POCKETD.TAIL1234.TS.NET.", false},
		{"http://example.org", true},
		{"http://192.0.2.1", true},
		{"http://10.0.0.5", true},
		{"http://192.168.1.10", true},
		{"http://100.128.0.1", true},
		{"http://100.63.255.255", true},
		{"http://[fd00::1]", true},
		{"http://ts.net", true},
		{"http://pocketd.ts.net.example.org", true},
		{"http://localhost.example.org", true},
		{"http://127.0.0.1.example.org", true},
	} {
		if got := NeedsHTTPWarning(tc.server); got != tc.warn {
			t.Errorf("NeedsHTTPWarning(%q) = %v, want %v", tc.server, got, tc.warn)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	for _, tc := range []struct {
		from, to string
		same     bool
	}{
		{"https://pocketd.example.ts.net:8443/v1/hello", "https://pocketd.example.ts.net:8443/v1/other", true},
		{"https://example.org/v1/hello", "https://EXAMPLE.org:443/v1/hello", true},
		{"http://example.org/v1/hello", "http://example.org:80/x", true},
		{"https://example.org/v1/hello", "http://example.org/v1/hello", false},
		{"https://example.org:8443/v1/hello", "http://example.org:8443/v1/hello", false},
		{"https://example.org/v1/hello", "https://example.org:8443/v1/hello", false},
		{"https://example.org/v1/hello", "https://evil.example.org/v1/hello", false},
		{"http://127.0.0.1:8788/v1/hello", "http://localhost:8788/v1/hello", false},
	} {
		from, _ := url.Parse(tc.from)
		to, _ := url.Parse(tc.to)
		if got := SameOrigin(from, to); got != tc.same {
			t.Errorf("SameOrigin(%s, %s) = %v, want %v", tc.from, tc.to, got, tc.same)
		}
	}
}

func TestBackoffDoublesToItsMaximum(t *testing.T) {
	b := Backoff{Min: time.Second, Max: 30 * time.Second}
	want := []time.Duration{0, time.Second, 2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second}
	for n, w := range want {
		if got := b.Delay(n); got != w {
			t.Errorf("Delay(%d) = %s, want %s", n, got, w)
		}
	}
	if got := b.Delay(1000); got != 30*time.Second {
		t.Errorf("Delay(1000) = %s, want the maximum", got)
	}
	if got := (Backoff{}).Delay(1); got != DefaultBackoff.Min {
		t.Errorf("the zero Backoff waits %s, want the default's %s", got, DefaultBackoff.Min)
	}
}

func TestRequestIDsKeepAFailedActionsID(t *testing.T) {
	n := 0
	ids := requestIDs{newID: func() string { n++; return strings.Repeat("x", n) }}
	first := ids.For("send:1", "hello")
	if again := ids.For("send:1", "hello"); again != first {
		t.Errorf("the same action tried again got %q, want its first id %q", again, first)
	}
	if changed := ids.For("send:1", "hello!"); changed == first {
		t.Error("a changed action kept the old id")
	}
	if other := ids.For("send:2", "hello!"); other == ids.For("send:1", "hello!") {
		t.Error("two keys share an id")
	}
	kept := ids.For("send:1", "hello!")
	ids.Done("send:1")
	if after := ids.For("send:1", "hello!"); after == kept {
		t.Error("an action sent again after a success kept the old id: the service would apply it once only")
	}
}

func TestModelChoicesHaveDistinctLabels(t *testing.T) {
	ref := func(p, id string) proto.ModelRef { return proto.ModelRef{Provider: p, ID: id} }
	got := ModelChoices([]proto.Model{
		{ModelRef: ref("anthropic", "claude-opus-5-5"), Name: "Claude Opus 5.5"},
		{ModelRef: ref("anthropic", "claude-opus-5-5-2026"), Name: "Claude Opus 5.5"},
		{ModelRef: ref("openai-codex", "gpt-5.5")},
	})
	want := []string{"Claude Opus 5.5 · anthropic", "anthropic/claude-opus-5-5-2026", "gpt-5.5 · openai-codex"}
	if len(got) != len(want) {
		t.Fatalf("choices %+v", got)
	}
	for i := range want {
		if got[i].Label != want[i] {
			t.Errorf("choice %d is %q, want %q", i, got[i].Label, want[i])
		}
	}
	if got[1].Ref != ref("anthropic", "claude-opus-5-5-2026") {
		t.Errorf("choice 1 starts %+v", got[1].Ref)
	}
}

func TestValidateMessage(t *testing.T) {
	if err := ValidateMessage("  \n\t", 0); err == nil {
		t.Error("white space passed")
	}
	if err := ValidateMessage(strings.Repeat("a", proto.MaxMessageSize+1), 0); err == nil {
		t.Error("a message over the limit passed")
	}
	if err := ValidateMessage(strings.Repeat("a", proto.MaxMessageSize), 0); err != nil {
		t.Errorf("a message at the limit failed: %v", err)
	}
	// Attached files stand for the text (rule 10a).
	if err := ValidateMessage("  ", 1); err != nil {
		t.Errorf("files alone failed: %v", err)
	}
	if err := ValidateMessage(strings.Repeat("a", proto.MaxMessageSize+1), 1); err == nil {
		t.Error("a message with files over the limit passed")
	}
}

func TestValidateNewSession(t *testing.T) {
	ok := proto.CreateSession{Cwd: "/home/team/projects/demo", Model: proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}}
	models := []proto.Model{{ModelRef: ok.Model, Reasoning: true, ThinkingLevels: []string{"off", "low", "high"}}}
	if err := ValidateNewSession(ok, models); err != nil {
		t.Fatalf("a good session failed: %v", err)
	}
	for name, change := range map[string]func(*proto.CreateSession){
		"no cwd":       func(r *proto.CreateSession) { r.Cwd = " " },
		"relative cwd": func(r *proto.CreateSession) { r.Cwd = "projects/demo" },
		"no model":     func(r *proto.CreateSession) { r.Model = proto.ModelRef{} },
		"long title":   func(r *proto.CreateSession) { r.Title = strings.Repeat("t", MaxTitleSize+1) },
		// The model lists its levels (protocol rule 8): one it lacks fails.
		"a level the model lacks": func(r *proto.CreateSession) { r.Thinking = "medium" },
	} {
		r := ok
		change(&r)
		if err := ValidateNewSession(r, models); err == nil {
			t.Errorf("%s passed", name)
		}
	}
	for _, level := range ThinkingChoices(models[0]) {
		r := ok
		r.Thinking = level
		if err := ValidateNewSession(r, models); err != nil {
			t.Errorf("thinking %q failed: %v", level, err)
		}
	}
	// Without the model's levels, the level is the service's to check.
	r := ok
	r.Thinking = "medium"
	if err := ValidateNewSession(r, nil); err != nil {
		t.Errorf("a level checked without the models: %v", err)
	}
}

func TestThinkingChoicesAreTheModelsLevels(t *testing.T) {
	m := proto.Model{ThinkingLevels: []string{"off", "minimal", "high"}}
	if got := strings.Join(ThinkingChoices(m), ","); got != ",off,minimal,high" {
		t.Errorf("choices %q, want the default, then the model's levels", got)
	}
	if got := ThinkingChoices(proto.Model{}); len(got) != 1 || got[0] != "" {
		t.Errorf("a model without levels offers %q, want only the default", got)
	}
}

// The New Session form offers "No profile", then each profile a session
// can start from, labeled "name · role"; a profile with a problem is
// listed by ProfileProblems instead. With a profile, its own model comes
// first, as a zero ref the router fills.
func TestProfileChoicesAndModels(t *testing.T) {
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	profiles := []proto.Profile{
		{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Runs: opus},
		{ID: "clerk", Problem: "no sign-in serves it"},
		{ID: "vera", Role: proto.RoleAgent, Runs: opus},
		{ID: "vera2", Name: "vera", Role: proto.RoleAgent, Runs: opus},
	}
	var labels []string
	for _, ch := range ProfileChoices(profiles) {
		labels = append(labels, ch.Label)
	}
	if want := []string{"No profile", "Odrade · supervisor", "vera · agent", "vera · agent (vera2)"}; !slices.Equal(labels, want) {
		t.Errorf("labels %q, want %q", labels, want)
	}
	if got := ProfileProblems(profiles); !slices.Equal(got, []string{"clerk: no sign-in serves it"}) {
		t.Errorf("problems %q", got)
	}
	models := []proto.Model{{ModelRef: opus, Name: "Claude Opus 5.5"}}
	choices := ModelChoicesFor(models, &profiles[0])
	if len(choices) != 2 || choices[0].Ref != (proto.ModelRef{}) || choices[0].Label != "The profile's model · anthropic/claude-opus-5-5" {
		t.Errorf("choices with a profile %+v", choices)
	}
	if got := ModelChoicesFor(models, nil); len(got) != 1 || got[0].Ref != opus {
		t.Errorf("choices without a profile %+v", got)
	}
}

// With a profile, an empty folder and a zero model are pocketd's to fill.
func TestValidateNewSessionWithAProfile(t *testing.T) {
	if err := ValidateNewSession(proto.CreateSession{Profile: "odrade"}, nil); err != nil {
		t.Errorf("a profile alone: %v", err)
	}
	if err := ValidateNewSession(proto.CreateSession{Profile: "odrade", Cwd: "agents"}, nil); err == nil {
		t.Error("a relative folder with a profile was accepted")
	}
	if err := ValidateNewSession(proto.CreateSession{Cwd: "/w"}, nil); err == nil {
		t.Error("no model and no profile was accepted")
	}
}

// A chosen profile pocketd no longer offers is kept, as gone.
func TestProfileChoicesKeepingAGoneProfile(t *testing.T) {
	opus := proto.ModelRef{Provider: "anthropic", ID: "claude-opus-5-5"}
	list := []proto.Profile{{ID: "odrade", Name: "Odrade", Role: proto.RoleSupervisor, Runs: opus}}
	got := ProfileChoicesKeeping(list, "vera", "Vera · agent")
	if last := got[len(got)-1]; !last.Gone || last.ID != "vera" || last.Label != "Vera · agent (no longer available)" || last.Profile != nil {
		t.Errorf("kept %+v", last)
	}
	if got := ProfileChoicesKeeping(list, "odrade", "Odrade · supervisor"); len(got) != 2 || got[1].Gone {
		t.Errorf("an offered profile %+v", got)
	}
	if got := ProfileChoicesKeeping(list, "", ""); len(got) != 2 {
		t.Errorf("nothing to keep %+v", got)
	}
}

// The summary of profiles that cannot start is one bounded line.
func TestProfileProblemSummaryIsBounded(t *testing.T) {
	var profiles []proto.Profile
	for i := range 13 {
		profiles = append(profiles, proto.Profile{ID: fmt.Sprintf("p%d", i), Problem: strings.Repeat("x", 300)})
	}
	got := ProfileProblemSummary(profiles, 3)
	if !strings.HasPrefix(got, "13 of the profiles cannot start: ") || !strings.Contains(got, "and 10 more") || len([]rune(got)) > 400 {
		t.Errorf("summary (%d runes): %q", len([]rune(got)), got)
	}
	if ProfileProblemSummary(profiles[:0], 3) != "" {
		t.Error("a summary with no problems")
	}
}

// The dialog's line names the profiles that cannot start by id, bounded.
func TestProfileProblemLine(t *testing.T) {
	var profiles []proto.Profile
	for i := range 13 {
		profiles = append(profiles, proto.Profile{ID: fmt.Sprintf("p%d", i), Problem: "x"})
	}
	if got := ProfileProblemLine(profiles, 3); got != "13 profiles cannot start: p0, p1, p2 and 10 more" {
		t.Errorf("line %q", got)
	}
	if got := ProfileProblemLine(profiles[:1], 3); got != "1 profile cannot start: p0" {
		t.Errorf("one %q", got)
	}
}
