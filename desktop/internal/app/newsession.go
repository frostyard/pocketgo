package app

import (
	"slices"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// newSessionForm is the New Session dialog's state.
type newSessionForm struct {
	open            bool
	cwd, title      string
	model, thinking string // the choices as the selects show them
	err             string
	// choices are the models offered, made from models, the store's
	// answer they were made from.
	choices []state.ModelChoice
	models  []proto.Model
}

// defaultThinking is how the thinking select shows the model's default.
const defaultThinking = "default"

// startNewSession opens the New Session dialog, with the open session's
// directory as the place to start.
func (a *App) startNewSession() {
	if f := a.newSession; f != nil && f.open {
		return
	}
	cwd := ""
	if s, ok := a.st.Data.Session(a.st.Data.OpenID()); ok {
		cwd = s.Cwd
	} else if list := a.st.Data.Sessions(); len(list) > 0 {
		cwd = list[0].Cwd
	}
	a.newSession = &newSessionForm{open: true, cwd: cwd, thinking: defaultThinking}
	// The models pocketd runs change as its providers sign in and out:
	// every opening asks again.
	a.st.LoadModels()
}

// modelChoices offers the models of the store's latest answer, keeping the
// model chosen while it is still offered.
func (f *newSessionForm) modelChoices(models []proto.Model) {
	if models == nil || slices.Equal(models, f.models) {
		return
	}
	f.models = models
	f.choices = state.ModelChoices(models)
	for _, ch := range f.choices {
		if ch.Label == f.model {
			return
		}
	}
	f.model = ""
	if len(f.choices) > 0 {
		f.model = f.choices[0].Label
	}
}

// labels are the choices' labels, for the select.
func (f *newSessionForm) labels() []string {
	out := make([]string, len(f.choices))
	for i, ch := range f.choices {
		out[i] = ch.Label
	}
	return out
}

// chosen is the model chosen.
func (f *newSessionForm) chosen() proto.ModelRef {
	for _, ch := range f.choices {
		if ch.Label == f.model {
			return ch.Ref
		}
	}
	return proto.ModelRef{}
}

func (a *App) newSessionDialog(c *ui.Context) {
	f := a.newSession
	if f == nil || !f.open {
		return
	}
	models, modelsErr := a.st.Models()
	f.modelChoices(models)
	busy := a.st.Creating().Busy
	ui.Modal(c, &f.open, func() {
		ui.Column(c).Width(540).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
			ui.Text(c, "New session").FontSize(18).Bold()
			frostui.Muted(c, "An agent session on the service host, in a working directory there.").Wrap()
			frostui.Label(c, "Working directory")
			cwd := ui.TextInput(c, &f.cwd).Label("Working directory").Placeholder("/home/team/projects/…").AutoFocus().Disabled(busy)
			frostui.Label(c, "Model")
			switch {
			case len(f.choices) > 0:
				ui.Select(c, &f.model, f.labels()).Label("Model").Disabled(busy)
			case modelsErr != "":
				frostui.Problem(c, "Loading the models failed: "+modelsErr).Wrap()
			case models != nil:
				frostui.Problem(c, "pocketd runs no model now: sign it in to a provider (pocketd login PROVIDER), then reload.").Wrap()
			default:
				frostui.Loading(c, "Loading the models…")
			}
			if len(f.choices) == 0 && (models != nil || modelsErr != "") {
				ui.Row(c).Children(func() {
					if ui.Button(c, "Reload Models").Clicked() {
						a.st.LoadModels()
					}
				})
			}
			frostui.Label(c, "Title (optional)")
			ui.TextInput(c, &f.title).Label("Title").Placeholder("Session <id>").Disabled(busy)
			frostui.Label(c, "Thinking")
			ui.Select(c, &f.thinking, thinkingChoices()).Label("Thinking").Disabled(busy)
			if f.err != "" {
				frostui.Problem(c, f.err).Wrap()
			}
			ui.Row(c).Gap(frostui.SpaceS).Justify(ui.End).Children(func() {
				if ui.Button(c, "Cancel").Disabled(busy).Clicked() {
					f.open = false
				}
				label := "Start Session"
				if busy {
					label = "Starting…"
				}
				if ui.PrimaryButton(c, label).Disabled(busy).Clicked() || cwd.Submitted() {
					a.createSession(f)
				}
			})
		})
	})
}

// thinkingChoices are state.ThinkingLevels as the select shows them.
func thinkingChoices() []string {
	out := make([]string, 0, len(state.ThinkingLevels))
	for _, level := range state.ThinkingLevels {
		if level == "" {
			level = defaultThinking
		}
		out = append(out, level)
	}
	return out
}

// createSession sends the form; the new session opens when pocketd made
// it, and the form stays with what went wrong when it did not.
func (a *App) createSession(f *newSessionForm) {
	if a.st.Creating().Busy {
		return
	}
	thinking := f.thinking
	if thinking == defaultThinking {
		thinking = ""
	}
	request := proto.CreateSession{Cwd: f.cwd, Title: f.title, Model: f.chosen(), Thinking: thinking}
	f.err = ""
	a.st.CreateSession(request, func(s proto.Session, err error) {
		if a.newSession != f {
			return
		}
		if err != nil {
			f.err = capitalize(err.Error())
			return
		}
		f.open = false
		a.openSession(s.ID)
	})
}
