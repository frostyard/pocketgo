package app

import (
	"slices"
	"strings"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// newSessionForm is the New Session dialog's state.
type newSessionForm struct {
	open                     bool
	title                    string
	profile, model, thinking string // the choices as the selects show them
	err                      string
	// choices are the models offered, made from models, the store's
	// answer they were made from, for the profile choicesFor.
	choices    []state.ModelChoice
	models     []proto.Model
	choicesFor string
	// profileChoices are the profiles offered, made from profiles;
	// appliedID is the profile the form last took its model from.
	profileChoices []state.ProfileChoice
	profiles       []proto.Profile
	appliedID      string
	// folder is the working directory, the repository and the worktree
	// switch, with what the form filled in itself (state.SessionFolder):
	// the first suggestion is the last session's folder. repoChoices are
	// the repositories offered for the profile chosen (ADR-0027).
	folder      state.SessionFolder
	repoChoices []state.RepositoryChoice
	// staleModels makes the model choices be made again, keeping the one
	// chosen: the profile's own model changed.
	staleModels bool
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
	a.newSession = &newSessionForm{open: true, folder: state.NewSessionFolder(cwd), thinking: defaultThinking, profile: state.NoProfile}
	// The models pocketd runs change as its providers sign in and out, and
	// the profiles as the owner edits them: every opening asks again.
	a.st.LoadModels()
	a.st.LoadProfiles()
}

// profileChoicesFrom offers the profiles of the store's latest answer,
// keeping the profile chosen while it is still offered, by its id.
func (f *newSessionForm) profileChoicesFrom(profiles []proto.Profile) {
	if profiles == nil || slices.EqualFunc(profiles, f.profiles, sameProfile) {
		return
	}
	was := f.selectedChoice()
	var before *proto.Profile
	if was != nil && was.Profile != nil {
		copied := *was.Profile
		before = &copied
	}
	f.profiles = slices.Clone(profiles)
	keepID, keepLabel := "", ""
	if was != nil {
		keepID, keepLabel = was.ID, strings.TrimSuffix(was.Label, " (no longer available)")
	}
	f.profileChoices = state.ProfileChoicesKeeping(f.profiles, keepID, keepLabel)
	f.profile = state.NoProfile
	for _, ch := range f.profileChoices {
		if was != nil && ch.ID == was.ID {
			f.profile = ch.Label
		}
	}
	// The same profile with another model: its own model follows, unless
	// the owner chose one (its folder follows in applyProfile).
	if now := f.selected(); now != nil && before != nil && now.ID == before.ID && now.Runs != before.Runs {
		f.staleModels = true
	}
}

// startOf is where a session from p starts by default: the folder pocketd
// lists for it (its home, else its workspace).
func startOf(p *proto.Profile) string {
	if p.Cwd != "" {
		return p.Cwd
	}
	return p.Home
}

// sameProfile reports whether a and b are the same profile, unchanged,
// whether it keeps a standing session, is private and which repositories
// it names where included: that decides what the form asks and sends.
func sameProfile(a, b proto.Profile) bool {
	return a.ID == b.ID && a.Name == b.Name && a.Role == b.Role && a.Runs == b.Runs && a.Home == b.Home && a.Cwd == b.Cwd && a.Problem == b.Problem &&
		a.Standing == b.Standing && a.CompactAt == b.CompactAt && a.Private == b.Private && slices.Equal(a.Repositories, b.Repositories)
}

// selectedChoice is the profile choice the select shows, nil before the
// profiles loaded.
func (f *newSessionForm) selectedChoice() *state.ProfileChoice {
	for i := range f.profileChoices {
		if f.profileChoices[i].Label == f.profile {
			return &f.profileChoices[i]
		}
	}
	return nil
}

// selected is the profile chosen, nil for none or one that is gone.
func (f *newSessionForm) selected() *proto.Profile {
	if ch := f.selectedChoice(); ch != nil {
		return ch.Profile
	}
	return nil
}

// applyProfile brings the folder in step with the chosen profile and its
// repositories (state.SessionFolder.Sync): its start folder, unless the
// owner typed another, whenever the choice or that folder changed, and the
// repository chosen, whose folder replaces it. A choice without a folder
// of its own (No profile, or a profile with neither home nor workspace)
// gets the original suggestion back, so no other profile's home is sent as
// this one's folder. A new choice also starts at the profile's own model.
func (f *newSessionForm) applyProfile() {
	id, start := "", ""
	ch := f.selectedChoice()
	if ch != nil {
		id = ch.ID
		if ch.Profile != nil {
			start = startOf(ch.Profile)
		}
	}
	f.repoChoices = state.RepositoryChoicesFor(ch, f.profiles)
	f.folder.Sync(id, start, f.repoChoices)
	if id != f.appliedID {
		f.appliedID = id
		f.choicesFor = "\x00"
	}
}

// repoLabels are the repository choices' labels, for the select.
func (f *newSessionForm) repoLabels() []string {
	out := make([]string, len(f.repoChoices))
	for i, ch := range f.repoChoices {
		out[i] = ch.Label
	}
	return out
}

// request is the CreateSession the form sends now (state.NewSessionRequest).
func (f *newSessionForm) request() proto.CreateSession {
	thinking := f.thinking
	if thinking == defaultThinking {
		thinking = ""
	}
	return state.NewSessionRequest(f.selected(), f.folder, f.title, f.chosen(), thinking)
}

// worktree is the folder and branch of the worktree the form's request
// makes, named from the request id it would go with now
// (state.WorktreePreview); ok is false when it makes none.
func (a *App) worktree(f *newSessionForm) (path, branch string, ok bool) {
	request := f.request()
	if !request.Worktree {
		return "", "", false
	}
	request.RequestID = a.st.NextCreateID(request)
	return state.WorktreePreview(request)
}

// modelChoices offers the models of the store's latest answer, keeping the
// model chosen while it is still offered. The model is the same model by
// its provider and id, not by its label: a label names another model when
// the models around it change (state.ModelChoices).
func (f *newSessionForm) modelChoices(models []proto.Model) {
	profile := f.selected()
	profileID := ""
	if profile != nil {
		profileID = profile.ID
	}
	if models == nil || slices.EqualFunc(models, f.models, sameModel) && f.choicesFor == profileID && !f.staleModels {
		return
	}
	f.staleModels = false
	// A new profile starts at its own model; otherwise the model chosen
	// stays while it is offered.
	newProfile := f.choicesFor != profileID
	was, chose := f.chosen(), f.model != "" && !newProfile
	f.models = models
	f.choicesFor = profileID
	f.choices = state.ModelChoicesFor(models, profile)
	f.model = ""
	for _, ch := range f.choices {
		if chose && ch.Ref == was {
			f.model = ch.Label
			return
		}
	}
	if len(f.choices) > 0 {
		f.model = f.choices[0].Label
	}
}

// sameModel reports whether a and b are the same model with the same
// name and thinking levels.
func sameModel(a, b proto.Model) bool {
	return a.ModelRef == b.ModelRef && a.Name == b.Name && a.Reasoning == b.Reasoning && slices.Equal(a.ThinkingLevels, b.ThinkingLevels)
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

// thinkingChoices are the thinking levels the chosen model takes
// (state.ThinkingChoices), as the select shows them. A level the model
// does not take goes back to the model's default.
func (f *newSessionForm) thinkingChoices() []string {
	ref := f.chosen()
	if p := f.selected(); ref == (proto.ModelRef{}) && p != nil {
		// The profile's own model: the one the router picks now.
		ref = p.Runs
	}
	i := slices.IndexFunc(f.models, func(m proto.Model) bool { return m.ModelRef == ref })
	levels := []string{""}
	if i >= 0 {
		levels = state.ThinkingChoices(f.models[i])
	}
	out := make([]string, 0, len(levels))
	for _, level := range levels {
		if level == "" {
			level = defaultThinking
		}
		out = append(out, level)
	}
	if !slices.Contains(out, f.thinking) {
		f.thinking = defaultThinking
	}
	return out
}

func (a *App) newSessionDialog(c *ui.Context) {
	f := a.newSession
	if f == nil || !f.open {
		return
	}
	profiles, profilesErr := a.st.Profiles()
	// A standing session the desktop knows opens whatever its profile's
	// problem (ADR-0026).
	profiles = state.OpenableProfiles(profiles, a.st.Data.Sessions())
	f.profileChoicesFrom(profiles)
	f.applyProfile()
	models, modelsErr := a.st.Models()
	f.modelChoices(models)
	busy := a.st.Creating().Busy
	ui.Modal(c, &f.open, func() {
		// The fields scroll when the window is too short for them, and the
		// buttons, with what went wrong, stay at the bottom at every size
		// (the owner's choice, 2026-11-20).
		label, working, submitted := "Start Session", "Starting…", false
		ui.Column(c).Width(a.fitWidth(c, 540)).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Shrink(1).MinHeight(0).Children(func() {
			ui.Text(c, "New session").FontSize(18).Bold()
			ui.Scroll(c).Grow(1).Shrink(1).MinHeight(0).Label("New session's fields").Children(func() {
				ui.Column(c).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
					label, working, submitted = a.newSessionFields(c, f, profiles, profilesErr, models, modelsErr, busy)
				})
			})
			if f.err != "" {
				frostui.Problem(c, f.err).Wrap()
			}
			a.newSessionButtons(c, f, busy, label, working, submitted)
		})
	})
}

// newSessionFields are the form's fields, in its scrolling part: the
// primary button's label, its label while busy, and whether the folder's
// input sent the form.
func (a *App) newSessionFields(c *ui.Context, f *newSessionForm, profiles []proto.Profile, profilesErr string, models []proto.Model, modelsErr string, busy bool) (label, working string, submitted bool) {
	frostui.Muted(c, "An agent session on the service host, in a working directory there.").Wrap()
	if len(f.profileChoices) > 1 {
		frostui.Label(c, "Profile")
		labels := make([]string, len(f.profileChoices))
		for i, ch := range f.profileChoices {
			labels[i] = ch.Label
		}
		ui.Select(c, &f.profile, labels).Label("Profile").Disabled(busy)
	}
	if line := state.ProfileProblemLine(profiles, 3); line != "" {
		// One line, the reasons in its tooltip.
		frostui.Muted(c, line).SingleLine().Tooltip(state.ProfileProblemSummary(profiles, 3))
	}
	if profilesErr != "" {
		frostui.Problem(c, "Loading the profiles failed: "+profilesErr).Wrap()
	}
	// A standing profile's session has its own folder, model and title:
	// the form opens it, and asks for none of them (ADR-0026). The choice
	// is read after the select, which may change it in this frame.
	if chosen := f.selected(); chosen != nil && chosen.Standing {
		frostui.Muted(c, state.StandingNote(*chosen)).Wrap()
		return "Open Session", "Opening…", false
	}
	// A repository fills in its folder; a session on one may start in a
	// new worktree, whose folder and branch show before it goes
	// (ADR-0027).
	if len(f.repoChoices) > 1 {
		frostui.Label(c, "Repository")
		ui.Select(c, &f.folder.Repository, f.repoLabels()).Label("Repository").Disabled(busy)
		if f.folder.Chosen().Name != "" {
			ui.Checkbox(c, &f.folder.Worktree, "In a new worktree").Disabled(busy)
		}
	}
	frostui.Label(c, "Working directory")
	if path, branch, ok := a.worktree(f); ok {
		frostui.Body(c, path).Wrap()
		frostui.Muted(c, state.WorktreeLine(branch)).Wrap()
	} else {
		cwd := ui.TextInput(c, &f.folder.Cwd).Label("Working directory").Placeholder("/home/team/projects/…").AutoFocus().Disabled(busy)
		submitted = cwd.Submitted()
	}
	frostui.Label(c, "Model")
	switch {
	case len(f.choices) > 0:
		ui.Select(c, &f.model, f.labels()).Label("Model").Disabled(busy)
	case modelsErr != "":
	case models != nil:
		frostui.Problem(c, "pocketd runs no model now: sign it in to a provider (pocketd login PROVIDER), then reload.").Wrap()
	default:
		frostui.Loading(c, "Loading the models…")
	}
	if modelsErr != "" {
		// The choices above, if any, are the last ones that loaded:
		// pocketd may no longer run them.
		msg := "Loading the models failed: " + modelsErr
		if len(f.choices) > 0 {
			msg = "Reloading the models failed, so this list may be out of date: " + modelsErr
		}
		frostui.Problem(c, msg).Wrap()
	}
	if len(f.choices) == 0 && models != nil || modelsErr != "" || profilesErr != "" {
		ui.Row(c).Children(func() {
			if ui.Button(c, "Reload Models").Clicked() {
				// Which profiles can start depends on the same sign-ins.
				a.st.LoadModels()
				a.st.LoadProfiles()
			}
		})
	}
	frostui.Label(c, "Title (optional)")
	ui.TextInput(c, &f.title).Label("Title").Placeholder("Session <id>").Disabled(busy)
	frostui.Label(c, "Thinking")
	ui.Select(c, &f.thinking, f.thinkingChoices()).Label("Thinking").Disabled(busy)
	return "Start Session", "Starting…", submitted
}

// newSessionButtons are the form's Cancel and its primary button, labeled
// label, or working while busy; submitted sends the form as the button
// does.
func (a *App) newSessionButtons(c *ui.Context, f *newSessionForm, busy bool, label, working string, submitted bool) {
	ui.Row(c).Gap(frostui.SpaceS).Justify(ui.End).Children(func() {
		if ui.Button(c, "Cancel").Disabled(busy).Clicked() {
			f.open = false
		}
		if busy {
			label = working
		}
		if ui.PrimaryButton(c, label).Disabled(busy).Clicked() || submitted {
			a.createSession(f)
		}
	})
}

// createSession sends the form; the new session opens when pocketd made
// it, and the form stays with what went wrong when it did not.
func (a *App) createSession(f *newSessionForm) {
	if a.st.Creating().Busy {
		return
	}
	if ch := f.selectedChoice(); ch != nil && ch.Gone {
		f.err = "pocketd no longer offers the profile " + ch.ID + ": choose another profile, or No profile"
		return
	}
	request := f.request()
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
