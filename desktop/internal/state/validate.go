package state

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/frostyard/pocketgo/proto"
)

// ThinkingChoices are the thinking levels a session on model m takes
// (protocol rule 8): "" for the model's default, then the levels the model
// lists, lowest first.
func ThinkingChoices(m proto.Model) []string {
	return append([]string{""}, m.ThinkingLevels...)
}

// MaxTitleSize is the longest session title, in bytes (protocol rule 9).
const MaxTitleSize = 200

// ModelChoice is a model as the New Session form offers it.
type ModelChoice struct {
	Label string
	Ref   proto.ModelRef
}

// ProfileChoice is a profile as the New Session form offers it; the first
// choice, "No profile", has an empty ID.
type ProfileChoice struct {
	Label string
	ID    string
	// Profile is nil for "No profile", and for a Gone choice.
	Profile *proto.Profile
	// Gone is a profile the owner chose that pocketd no longer offers: it
	// stays chosen, and nothing starts from it (ProfileChoicesKeeping).
	Gone bool
}

// ProfileChoicesKeeping are ProfileChoices, keeping a profile the owner
// chose, by its id and label, when pocketd no longer offers it: as a Gone
// choice, so the form never quietly falls back to no profile, which would
// start an agent with every tool in place of the one chosen.
func ProfileChoicesKeeping(profiles []proto.Profile, keepID, keepLabel string) []ProfileChoice {
	out := ProfileChoices(profiles)
	if keepID == "" || slices.ContainsFunc(out, func(ch ProfileChoice) bool { return ch.ID == keepID }) {
		return out
	}
	return append(out, ProfileChoice{Label: keepLabel + " (no longer available)", ID: keepID, Gone: true})
}

// NoProfile is the label of the choice of no profile.
const NoProfile = "No profile"

// ProfileChoices are "No profile", then the profiles a session can start
// from (GET /v1/profiles) in its order, each labeled "name · role", and
// "name · role · standing session" for a profile that keeps one. A profile
// with a problem is not offered: ProfileProblems says why.
func ProfileChoices(profiles []proto.Profile) []ProfileChoice {
	out := []ProfileChoice{{Label: NoProfile}}
	taken := map[string]bool{NoProfile: true}
	for i := range profiles {
		p := &profiles[i]
		if p.Problem != "" {
			continue
		}
		name := p.Name
		if name == "" {
			name = p.ID
		}
		label := name + " · " + string(p.Role)
		if p.Standing {
			// New Session opens its one session (ADR-0026).
			label += " · standing session"
		}
		if taken[label] {
			label += " (" + p.ID + ")"
		}
		taken[label] = true
		out = append(out, ProfileChoice{Label: label, ID: p.ID, Profile: p})
	}
	return out
}

// ProfileProblemLine is one short line naming the profiles a session cannot
// start from, by id, at most max of them, then how many more; "" for none.
// The New Session dialog shows it, and ProfileProblemSummary as its
// tooltip: the dialog does not scroll.
func ProfileProblemLine(profiles []proto.Profile, max int) string {
	var ids []string
	for _, p := range profiles {
		if p.Problem != "" {
			ids = append(ids, p.ID)
		}
	}
	if len(ids) == 0 {
		return ""
	}
	shown := strings.Join(ids[:min(len(ids), max)], ", ")
	if more := len(ids) - min(len(ids), max); more > 0 {
		shown += fmt.Sprintf(" and %d more", more)
	}
	if len(ids) == 1 {
		return "1 profile cannot start: " + shown
	}
	return fmt.Sprintf("%d profiles cannot start: %s", len(ids), shown)
}

// ProfileProblemSummary is one bounded line about the profiles a session
// cannot start from: at most max of them, each "id: problem" cut to 80
// characters, then how many more; "" for none.
func ProfileProblemSummary(profiles []proto.Profile, max int) string {
	problems := ProfileProblems(profiles)
	if len(problems) == 0 {
		return ""
	}
	shown := problems[:min(len(problems), max)]
	parts := make([]string, len(shown))
	for i, p := range shown {
		if r := []rune(p); len(r) > 80 {
			p = string(r[:79]) + "…"
		}
		parts[i] = p
	}
	text := strings.Join(parts, "; ")
	if more := len(problems) - len(shown); more > 0 {
		text += fmt.Sprintf("; and %d more (pocketd profiles lists them all)", more)
	}
	return fmt.Sprintf("%d of the profiles cannot start: %s", len(problems), text)
}

// ProfileProblems are the profiles a session cannot start from, as
// "id: problem".
func ProfileProblems(profiles []proto.Profile) []string {
	var out []string
	for _, p := range profiles {
		if p.Problem != "" {
			out = append(out, p.ID+": "+p.Problem)
		}
	}
	return out
}

// ModelChoicesFor are ModelChoices, after a first choice of the profile's
// own model when there is a profile: its Ref is zero, so pocketd's router
// chooses, and its label names the model the router picks now.
func ModelChoicesFor(models []proto.Model, profile *proto.Profile) []ModelChoice {
	choices := ModelChoices(models)
	if profile == nil {
		return choices
	}
	return append([]ModelChoice{{Label: "The profile's model · " + profile.Runs.String()}}, choices...)
}

// ModelChoices are the models pocketd runs now (GET /v1/models), in its
// order, each labeled "name · provider" ("id · provider" without a name),
// and "provider/id" where that label is taken already, so no two share a
// label.
func ModelChoices(models []proto.Model) []ModelChoice {
	out := make([]ModelChoice, 0, len(models))
	taken := map[string]bool{}
	for _, m := range models {
		label := m.Name
		if label == "" {
			label = m.ID
		}
		label += " · " + m.Provider
		if taken[label] {
			label = m.String()
		}
		taken[label] = true
		out = append(out, ModelChoice{Label: label, Ref: m.ModelRef})
	}
	return out
}

// ValidateMessage checks a message before it is sent, as the service would
// (protocol rule 10): 1 to proto.MaxMessageSize bytes, not only white
// space.
func ValidateMessage(text string) error {
	if strings.TrimSpace(text) == "" {
		return errors.New("the message is empty")
	}
	if len(text) > proto.MaxMessageSize {
		return fmt.Errorf("the message has %d bytes, more than the %d a message may have", len(text), proto.MaxMessageSize)
	}
	return nil
}

// ValidateAnswer checks the owner's answer to an ask as pocketd would: 1
// to proto.MaxAnswer characters once trimmed.
func ValidateAnswer(text string) error {
	text = strings.TrimSpace(text)
	if text == "" {
		return errors.New("the answer is empty")
	}
	if n := utf8.RuneCountInString(text); n > proto.MaxAnswer {
		return fmt.Errorf("the answer has %d characters, more than the %d an answer may have", n, proto.MaxAnswer)
	}
	return nil
}

// ValidateNewSession checks a new session before it is created, as the
// service would (protocol rules 6 to 9), except what only the service host
// knows: whether the directory exists, and which models it runs now. The
// thinking level is checked against the model's levels when models, the
// service's last answer, has the model.
func ValidateNewSession(request proto.CreateSession, models []proto.Model) error {
	cwd := strings.TrimSpace(request.Cwd)
	// With a profile, an empty folder is its home and a zero model the
	// router's choice (protocol rule 7b).
	profile := request.Profile != ""
	switch {
	case cwd == "" && !profile:
		return errors.New("enter the working directory, an absolute path on the service host")
	case cwd != "" && !path.IsAbs(cwd):
		return fmt.Errorf("the working directory %q is not an absolute path on the service host (it starts with /)", cwd)
	case (request.Model.Provider == "" || request.Model.ID == "") && !profile:
		return errors.New("choose a model")
	case len(request.Title) > MaxTitleSize:
		return fmt.Errorf("the title has %d bytes, more than the %d a title may have", len(request.Title), MaxTitleSize)
	}
	i := slices.IndexFunc(models, func(m proto.Model) bool { return m.ModelRef == request.Model })
	if i >= 0 && !slices.Contains(ThinkingChoices(models[i]), request.Thinking) {
		return fmt.Errorf("%s does not take the thinking level %q; it takes %s", request.Model, request.Thinking, strings.Join(models[i].ThinkingLevels, ", "))
	}
	return nil
}
