package state

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

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

// ValidateNewSession checks a new session before it is created, as the
// service would (protocol rules 6 to 9), except what only the service host
// knows: whether the directory exists, and which models it runs now. The
// thinking level is checked against the model's levels when models, the
// service's last answer, has the model.
func ValidateNewSession(request proto.CreateSession, models []proto.Model) error {
	cwd := strings.TrimSpace(request.Cwd)
	switch {
	case cwd == "":
		return errors.New("enter the working directory, an absolute path on the service host")
	case !path.IsAbs(cwd):
		return fmt.Errorf("the working directory %q is not an absolute path on the service host (it starts with /)", cwd)
	case request.Model.Provider == "" || request.Model.ID == "":
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
