package nsgoclient

import (
	"reflect"
	"slices"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

func (c *client) sessionDetails(cx *core.Scope) core.Node {
	id := c.session.Get()
	c.store.LoadChanges(id)
	d := &c.store.Data
	session := func() proto.Session { c.revision.Get(); s, _ := d.Session(id); return s }
	current := core.Untracked(cx, session)
	name := core.NewSignal(cx, current.Title)
	pin := ui.Button{Variant: ui.ButtonSecondary, TextFunc: func() string {
		c.revision.Get()
		if c.prefs.Pinned.Has(id) {
			return "Unpin session"
		}
		return "Pin session"
	}, OnPress: func() { c.prefs.Pinned = c.prefs.Pinned.Toggle(id); c.savePrefs(); c.changed() }}
	edit := core.Show{When: func() bool { c.revision.Get(); return d.Renamable(id) }, Then: func(*core.Scope) core.Node {
		return workspaceui.Stack(ui.Input{Label: "Session title", Value: name.Get, OnChange: name.Set}, c.button("Rename", func() {
			if err := state.ValidateTitle(name.Get()); err != nil {
				c.result(err)
				return
			}
			c.store.Rename(id, name.Get())
		}), core.Text{ContentFunc: func() string { c.revision.Get(); return c.store.Renaming(id).Err }})
	}}
	type relation struct {
		id    int64
		label string
	}
	relations := core.NewMemoFunc(cx, func() []relation {
		var links []relation
		c.revision.Get()
		if header, ok := d.WorkerHeaderOf(id); ok && header.Parent != 0 {
			links = append(links, relation{header.Parent, "Back to " + header.ParentTitle})
		} else if parent, ok := d.ParentOf(id); ok {
			links = append(links, relation{parent.ID, "Parent · " + sessionName(parent)})
		}
		for _, w := range d.WorkersOf(id) {
			links = append(links, relation{w.ID, state.WorkerCardLabel(w)})
		}
		return links
	}, slices.Equal)
	workers := core.Dynamic(func(*core.Scope) core.Node {
		var nodes []core.Node
		for _, r := range relations.Get() {
			nodes = append(nodes, c.button(r.label, func() { c.openSession(r.id) }))
		}
		if len(nodes) == 0 {
			return nil
		}
		return workspaceui.InspectorGroup("Workers", nodes...)
	})
	type changesView struct {
		files  state.SessionChanges
		loaded bool
		diff   state.ShownDiff
	}
	changedFiles := core.NewMemoFunc(cx, func() changesView {
		c.revision.Get()
		ch, ok := d.Changes(id)
		diff, _ := d.Diff()
		if diff.Session != id {
			diff = state.ShownDiff{}
		}
		return changesView{ch, ok, diff}
	}, func(a, b changesView) bool { return reflect.DeepEqual(a, b) })
	changes := core.Dynamic(func(*core.Scope) core.Node {
		v := changedFiles.Get()
		ch, ok := v.files, v.loaded
		summary := state.ChangesSummary(ch, ok)
		if summary == "" {
			if ok && ch.Status == state.ListLoaded {
				return nil
			}
			summary = "Reading changes…"
		}
		out := []core.Node{body(summary)}
		if ch.Err != "" {
			out = append(out, body(ch.Err))
		}
		for _, f := range ch.Changes.Files {
			if state.CanDiff(f) {
				out = append(out, c.button(state.FileName(f)+" "+state.FileCounts(f), func() { c.store.ShowDiff(id, f.Path) }))
			} else {
				out = append(out, body(state.FileName(f)))
			}
		}
		if diff := v.diff; diff.Session == id {
			out = append(out, title(diff.Path), body(diff.Err), core.Text{Content: diff.Diff.Diff, Style: ui.TypeCode, Selectable: core.SelectableOn}, body(state.CutNote(diff.Diff)), c.button("Close diff", d.CloseDiff))
		}
		return workspaceui.InspectorGroup("Changes", out...)
	})
	startOver := core.Show{When: func() bool { s := session(); ro, _ := d.ReadOnly(id); return s.Standing && !ro }, Then: func(*core.Scope) core.Node {
		why := func() string { return state.StartOverBlocked(session(), d.Open()) }
		return workspaceui.Stack(ui.Button{Text: "Start over", Disabled: func() bool { return why() != "" }, OnPress: func() {
			c.destroy("Start over?", state.StartOverQuestion(session()), "Start over", func() { c.store.StartOver(id) })
		}}, core.Text{ContentFunc: why})
	}}
	return workspaceui.Page(core.Text{Style: ui.TypeTitle, ContentFunc: func() string { return sessionName(session()) }},
		workspaceui.InspectorGroup("Session", pin, edit, startOver),
		workspaceui.InspectorGroup("Workspace", workspaceui.InspectorRow("Directory", func() string { return session().Cwd }, c.button("Open terminal here", func() { c.startTerminal(state.TerminalPlace{Session: id}) })),
			workspaceui.InspectorRow("Agent", func() string { return session().Profile })),
		workspaceui.InspectorGroup("Usage", workspaceui.InspectorRow("Model", func() string { s := session(); return strings.Join(nonempty(s.Model.ID, s.Thinking, s.Route), " · ") }),
			workspaceui.InspectorRow("Tokens", func() string {
				if used := state.UsageLine(session().Usage); used != "" {
					return used
				}
				return "No usage reported"
			}), workspaceui.InspectorRow("Context", func() string { c.revision.Get(); return d.FillOf(id).Tooltip() })),
		workers, changes)
}
func (c *client) search(cx *core.Scope) core.Node {
	query := core.NewSignal(cx, c.store.Data.Search().Query)
	run := func(s string) { c.store.Search(s); c.changed() }
	return workspaceui.Page(ui.Input{Label: "Search all sessions", Placeholder: "Search titles and transcripts", Value: query.Get, OnChange: query.Set, OnSubmit: run}, c.button("Search sessions", func() { run(query.Get()) }), core.Dynamic(func(*core.Scope) core.Node {
		c.revision.Get()
		found := c.store.Data.Search()
		groups := c.store.Data.SearchGroups()
		var out []core.Node
		switch {
		case found.Query == "":
			return core.View{Style: core.Style{Height: core.Pt(200)}, Children: []core.Node{workspaceui.EmptyState{Icon: lucide.Search, Title: "Search every session",
				Hint: "Find a past conversation by what was said in it: your messages, the agents' replies, reports and briefs, and the names of the tools they called."}}}
		case found.Loading:
			out = append(out, body("Searching…"))
		case found.Status == state.ListFailed:
			// A search that failed says so: it does not say nothing was found.
			return c.problemBanner(workspaceui.Danger, "Searching failed: "+found.Problem, func() { c.store.Search(found.Query) })
		case found.Answered != "" && len(found.Results.Hits) == 0:
			out = append(out, body(state.IndexingNote(found.Results)), core.View{Style: core.Style{Height: core.Pt(200)}, Children: []core.Node{
				workspaceui.EmptyState{Icon: lucide.SearchX, Title: "Nothing found", Hint: "No session says “" + found.Answered + "”."}}})
			return workspaceui.Stack(out...)
		case found.Answered != "":
			out = append(out, body(state.SearchNote(found.Results, len(groups))), body(state.IndexingNote(found.Results)))
		}
		now := time.Now()
		for _, g := range groups {
			// A session's hits are its card: its title, and its agent or
			// folder as the sidebar says it.
			head := []core.Node{core.Text{Content: g.Title, MaxLines: 1, Style: ui.TypeTitle.Merge(core.Style{FlexShrink: 1})}}
			if g.Tag != "" {
				head = append(head, core.Text{Content: g.Tag, MaxLines: 1, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)})
			}
			card := []core.Node{core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS}, Children: head}}
			for _, h := range g.Hits {
				card = append(card, c.searchHit(cx, g, h, now))
			}
			out = append(out, workspaceui.Card{Children: card})
		}
		return workspaceui.Stack(out...)
	}))
}

// searchHit is one hit: its snippet with its matches marked, and where and
// when it was said; a press opens its session at its entry.
func (c *client) searchHit(cx *core.Scope, g state.SearchGroup, h proto.SearchHit, now time.Time) core.Node {
	colors := ui.ColorsOf(cx)
	var spans []core.Span
	for _, sp := range state.SnippetSpans(h.Snippet, h.Marks) {
		span := core.Span{Content: sp.Text}
		if sp.Marked {
			span.Style = core.Style{FontWeight: core.FontWeightSemibold, CornerRadius: ui.RadiusSmall}
			span.StyleFunc = func() core.Style { return core.Style{Background: ui.Mix(colors.Surface(), colors.Accent(), 0.22)} }
		}
		spans = append(spans, span)
	}
	where := state.HitField(h.Field)
	if !h.At.IsZero() {
		where += " · " + state.ClockAt(h.At, now)
	}
	open := func(core.PressEvent) {
		c.goTo(destination{page: "Session", session: h.SessionID, entry: h.EntryID})
	}
	return core.View{Role: core.RoleButton, Label: g.Title + ": " + h.Snippet, OnPress: open,
		Style: core.Style{Gap: workspaceui.SpaceXS, Padding: core.Symmetric(workspaceui.SpaceXS, workspaceui.SpaceS), CornerRadius: ui.RadiusInset},
		Children: []core.Node{core.Text{Spans: spans},
			core.Text{Content: where, MaxLines: 1, Style: ui.TypeCaption, StyleFunc: ui.Colored(colors.Muted)}}}
}

func (c *client) newSession(cx *core.Scope) core.Node {
	name := core.NewSignal(cx, "")
	id := core.NewSignal(cx, "")
	model := core.NewSignal(cx, proto.ModelRef{})
	thinking := core.NewSignal(cx, "")
	cwd := ""
	if prev, ok := c.store.Data.Session(c.session.Get()); ok {
		cwd = prev.Cwd
	}
	folder := state.NewSessionFolder(cwd)
	edited := core.NewSignal(cx, 0)
	update := func() { edited.Update(func(i int) int { return i + 1 }) }
	profile := func() (*proto.Profile, []proto.Profile) {
		c.revision.Get()
		pp, _ := c.store.Profiles()
		pp = state.OpenableProfiles(pp, c.store.Data.Sessions())
		for _, p := range pp {
			if p.ID == id.Get() {
				return &p, pp
			}
		}
		return nil, pp
	}
	syncFolder := func() {
		p, pp := profile()
		start := ""
		if p != nil {
			start = p.Cwd
			if start == "" {
				start = p.Home
			}
		}
		folder.Sync(id.Get(), start, state.RepositoryChoices(p, pp))
	}
	submit := func() {
		p, _ := profile()
		if id.Get() != "" && p == nil {
			c.showNotice(ui.ToastWarning, "Session unavailable", "pocketd no longer offers the profile "+id.Get()+": choose another profile, or No profile")
			return
		}
		if p != nil && p.Problem != "" {
			c.showNotice(ui.ToastError, "Couldn’t start session", p.Problem)
			return
		}
		syncFolder()
		request := state.NewSessionRequest(p, folder, name.Get(), model.Get(), thinking.Get())
		c.store.CreateSession(request, func(s proto.Session, err error) {
			if err != nil {
				c.result(err)
				return
			}
			c.openSession(s.ID)
		})
		c.changed()
	}
	return workspaceui.Page(title("Create a session"), body("Profile"), ui.Select[string]{Value: id.Get, OptionsFunc: func() []ui.Option[string] {
		_, pp := profile()
		out := []ui.Option[string]{}
		for _, p := range state.ProfileChoices(pp) {
			out = append(out, ui.Option[string]{Value: p.ID, Label: p.Label})
		}
		return out
	}, OnChange: func(s string) { id.Set(s); model.Set(proto.ModelRef{}); thinking.Set(""); syncFolder(); update() }},
		c.profileNotes(),
		core.Show{When: func() bool { p, _ := profile(); return p == nil || !p.Standing }, Then: func(*core.Scope) core.Node {
			return workspaceui.Stack(ui.Input{Label: "Title", Placeholder: "Session title", Value: name.Get, OnChange: name.Set}, body("Repository"), ui.Select[string]{Value: func() string { edited.Get(); return folder.Repository }, OptionsFunc: func() []ui.Option[string] {
				p, pp := profile()
				out := []ui.Option[string]{}
				for _, r := range state.RepositoryChoices(p, pp) {
					out = append(out, ui.Option[string]{Value: r.Label, Label: r.Label})
				}
				return out
			}, OnChange: func(s string) { folder.Repository = s; syncFolder(); update() }},
				// A session in a new worktree shows the worktree's folder
				// and branch before it goes, named from the request id it
				// would go with now; any other has the folder to type.
				body("Working directory"), core.Dynamic(func(cx *core.Scope) core.Node {
					edited.Get()
					name.Get()
					model.Get()
					thinking.Get()
					c.revision.Get()
					p, _ := profile()
					request := state.NewSessionRequest(p, folder, name.Get(), model.Get(), thinking.Get())
					if request.Worktree {
						request.RequestID = c.store.NextCreateID(request)
						if path, branch, ok := state.WorktreePreview(request); ok {
							return workspaceui.Stack(core.Text{Content: path, Selectable: core.SelectableOn},
								core.Text{Content: state.WorktreeLine(branch), Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)})
						}
					}
					return ui.Input{Label: "Working directory", Placeholder: "Absolute path on the service host", Value: func() string { edited.Get(); return folder.Cwd }, OnChange: func(s string) { folder.Cwd = s; syncFolder(); update() }}
				}), workspaceui.Row(body("New worktree"), ui.Switch{Label: "New worktree", Checked: func() bool { edited.Get(); return folder.Worktree }, Disabled: func() bool { edited.Get(); return folder.Chosen().Name == "" }, OnChange: func(b bool) { folder.Worktree = b; update() }}),
				body("Model"), ui.Select[proto.ModelRef]{Searchable: true, Placeholder: "Choose model", Value: model.Get, OptionsFunc: func() []ui.Option[proto.ModelRef] {
					p, _ := profile()
					mm, _ := c.store.Models()
					out := []ui.Option[proto.ModelRef]{}
					for _, m := range state.ModelChoicesFor(mm, p) {
						out = append(out, ui.Option[proto.ModelRef]{Label: m.Label, Value: m.Ref})
					}
					return out
				}, OnChange: func(m proto.ModelRef) { model.Set(m); thinking.Set("") }}, ui.Select[string]{Placeholder: "Thinking level", Value: thinking.Get, OptionsFunc: func() []ui.Option[string] {
					c.revision.Get()
					out := []ui.Option[string]{{Value: "", Label: "Default thinking"}}
					mm, _ := c.store.Models()
					for _, m := range mm {
						if m.ModelRef == model.Get() {
							for _, l := range state.ThinkingChoices(m) {
								if l != "" {
									out = append(out, ui.Option[string]{Value: l, Label: l})
								}
							}
						}
					}
					return out
				}, OnChange: thinking.Set}, c.modelNotes(profile))
		}}, core.Dynamic(func(cx *core.Scope) core.Node {
			// A standing profile keeps one session: the form opens it,
			// and asks for no folder, model or title.
			if p, _ := profile(); p != nil && p.Standing {
				return core.Text{Content: state.StandingNote(*p), StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}
			}
			return core.Group{}
		}), ui.Button{TextFunc: func() string {
			if p, _ := profile(); p != nil && p.Standing {
				return "Open session"
			}
			return "Start session"
		}, OnPress: submit, Busy: func() bool { c.revision.Get(); return c.store.Creating().Busy }})
}

// profileNotes say, under New session's profile choice, which profiles
// cannot start and why, and that the profiles failed to load.
func (c *client) profileNotes() core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		c.revision.Get()
		profiles, problem := c.store.Profiles()
		var out []core.Node
		if line := state.ProfileProblemLine(profiles, 3); line != "" {
			out = append(out, ui.Tooltip{Text: state.ProfileProblemSummary(profiles, 3),
				Trigger: core.Text{Content: line, MaxLines: 1, Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}})
		}
		if problem != "" {
			out = append(out, c.listProblem("the profiles", problem, "", c.store.LoadProfiles))
		}
		return core.Group(out)
	})
}

// modelNotes say, under New session's model choice, that the models are
// loading, that pocketd runs none, or that loading them failed, with a
// way to load them again. Which profiles can start depends on the same
// sign-ins, so Reload Models loads the profiles too.
func (c *client) modelNotes(profile func() (*proto.Profile, []proto.Profile)) core.Node {
	return core.Dynamic(func(cx *core.Scope) core.Node {
		c.revision.Get()
		p, _ := profile()
		models, problem := c.store.Models()
		choices := len(state.ModelChoicesFor(models, p))
		reload := func() { c.store.LoadModels(); c.store.LoadProfiles() }
		switch {
		case problem != "" && choices > 0:
			return c.problemBanner(workspaceui.Warning, "Reloading the models failed, so this list may be out of date: "+problem, reload)
		case problem != "":
			return c.problemBanner(workspaceui.Danger, "Loading the models failed: "+problem, reload)
		case choices == 0 && models != nil:
			return c.problemBanner(workspaceui.Danger, "pocketd runs no model now: sign it in to a provider (pocketd login PROVIDER), then reload.", reload)
		case choices == 0:
			return core.Text{Content: "Loading the models…", Style: ui.TypeCaption, StyleFunc: ui.Colored(ui.ColorsOf(cx).Muted)}
		}
		return core.Group{}
	})
}
