package state

import (
	"errors"
	"fmt"
	"path"
	"slices"
	"strings"

	"github.com/frostyard/pocketgo/proto"
)

// ThinkingLevels are the thinking levels a session takes (protocol rule
// 8), "" for the model's default.
var ThinkingLevels = []string{"", "minimal", "low", "medium", "high", "xhigh"}

// MaxTitleSize is the longest session title, in bytes (protocol rule 9).
const MaxTitleSize = 200

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
// knows: whether the directory exists, and which models it runs now.
func ValidateNewSession(request proto.CreateSession) error {
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
	case !slices.Contains(ThinkingLevels, request.Thinking):
		return fmt.Errorf("the thinking level %q is not one of %s", request.Thinking, strings.Join(ThinkingLevels[1:], ", "))
	}
	return nil
}
