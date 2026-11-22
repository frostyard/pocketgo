package app

import (
	"strings"
	"time"

	"github.com/egoist/mygo/ui"
	"github.com/egoist/mygo/ui/mobile"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// The Search view finds sessions by what was said in them (ADR-0050):
// Ctrl+Shift+F (Cmd+Shift+F on a Mac), Search Sessions… in the command
// palette, and the sidebar filter's last row open it. Its hits group by
// session (state.Data.SearchGroups), and a hit opens its session scrolled
// to its entry, which shows highlighted for a moment (state.Seek). Its
// rules are state's; this file draws them.

// searchView is the Search view's state: its field, and whether the field
// takes the keyboard focus on the next frame.
type searchView struct {
	query string
	focus bool
}

// The highlight of the entry a hit opens: shown at once, held, then gone
// in a fade.
const (
	flashHold = 1200 * time.Millisecond
	flashFade = 600 * time.Millisecond
)

// searchCommand opens the Search view (ADR-0050).
func (a *App) searchCommand() frostui.Command {
	return frostui.Command{ID: "search", Title: "Search Sessions…", Group: "Go to", Icon: "system-search",
		Bindings: []frostui.KeyBinding{{Mods: ui.Cmd | ui.Shift, Key: ui.KeyF, Scope: frostui.Everywhere}},
		Run:      func() { a.goSearch("") }}
}

// goSearch shows the Search view, its field focused, and searches for q
// when it holds anything: the words the sidebar's filter offers.
func (a *App) goSearch(q string) {
	a.leaveSession()
	a.place = placeSearch
	a.st.Open(0)
	if q = strings.TrimSpace(q); q != "" {
		a.search.query = q
		a.st.Search(q)
	}
	a.search.focus = true
	a.showInSidebar()
}

// openHit shows session id scrolled to entry, 0 for a title's hit, which
// shows it where it opens.
func (a *App) openHit(id, entry int64) {
	a.openSession(id)
	if entry != 0 {
		a.tr.seek = state.Seek{Session: id, Entry: entry}
	}
}

// searchBar is the main bar over the Search view: its title, and how many
// hits it shows.
func (a *App) searchBar(c *ui.Context) {
	s := a.st.Data.Search()
	ui.Column(c).Grow(1).Children(func() {
		ui.Text(c, "Search").Bold().SingleLine()
		frostui.Caption(c, state.SearchNote(s.Results, len(a.st.Data.SearchGroups()))).SingleLine()
	})
}

// searchPane is the Search view: its field, then the hits grouped by
// session, or what stands in their place.
func (a *App) searchPane(c *ui.Context) {
	t := c.Theme()
	s := a.st.Data.Search()
	groups := a.st.Data.SearchGroups()
	ui.Column(c).Grow(1).Basis(0).MinHeight(0).AlignItems(ui.Stretch).Background(t.Background).Children(func() {
		// A phone's field and lists sit as the Sessions tab's do.
		side := a.gutter()
		if a.compact {
			side = frostui.SpaceL
		}
		ui.Column(c).AlignItems(ui.Center).Padding(frostui.SpaceM, side, frostui.SpaceS, side).Shrink(0).Children(func() {
			a.searchField(c)
		})
		ui.Scroll(c).Grow(1).Basis(0).MinHeight(0).Fill().Children(func() {
			side := a.gutter()
			if a.compact {
				side = 0
			}
			ui.Column(c).AlignItems(ui.Center).Padding(frostui.SpaceS, side, frostui.SpaceL, side).Children(func() {
				ui.Column(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceM).AlignItems(ui.Stretch).Children(func() {
					a.searchResults(c, s, groups)
				})
			})
		})
	})
}

// searchField is the Search view's field, with its Search button on a
// desktop's window; Enter searches too.
func (a *App) searchField(c *ui.Context) {
	ui.Row(c).MaxWidth(860).FillWidth().Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
		field := ui.SearchField(c, &a.search.query).Placeholder("Search all sessions").Label("Search all sessions").Grow(1)
		// The field takes the focus as the view opens, but on a compact
		// window, where a keyboard would come up over the hits.
		if a.search.focus {
			if !a.compact {
				field.Focus()
			}
			a.search.focus = false
		}
		if field.Submitted() {
			a.st.Search(a.search.query)
		}
		if !a.compact && ui.Button(c, "Search").Disabled(strings.TrimSpace(a.search.query) == "").Clicked() {
			a.st.Search(a.search.query)
		}
	})
}

// searchResults are the hits of search s, grouped, or what stands in their
// place: a hint before any search, the search at work, its failure, or
// that it found nothing.
func (a *App) searchResults(c *ui.Context, s state.Search, groups []state.SearchGroup) {
	if note := state.IndexingNote(s.Results); note != "" && s.Status == state.ListLoaded {
		frostui.Muted(c, note).Wrap()
	}
	switch {
	case s.Query == "":
		frostui.Empty(c, "system-search", "Search every session",
			"Find a past conversation by what was said in it: your messages, the agents' replies, reports and briefs, and the names of the tools they called.")
		return
	case s.Status == state.ListLoading || (s.Loading && s.Answered != s.Query):
		frostui.Loading(c, "Searching…")
		return
	case s.Status == state.ListFailed:
		ui.Row(c).Gap(frostui.SpaceM).AlignItems(ui.Center).Children(func() {
			frostui.Problem(c, "Searching failed: "+s.Problem).Wrap().Grow(1)
			if ui.Button(c, "Try Again").Clicked() {
				a.st.Search(s.Query)
			}
		})
		return
	case len(s.Results.Hits) == 0:
		frostui.Empty(c, "edit-find", "Nothing found", "No session says “"+s.Answered+"”.")
		return
	}
	if a.compact {
		ui.Row(c).Padding(0, frostui.SpaceL).Children(func() {
			frostui.Caption(c, state.SearchNote(s.Results, len(groups))).Wrap()
		})
	}
	now := a.opts.Now()
	for _, g := range groups {
		if a.compact {
			mobile.Section(c.Key(g.Session), g.Title, func() {
				for _, h := range g.Hits {
					a.hitRow(c, g, h, now)
				}
			})
			continue
		}
		frostui.Card(c.Key(g.Session)).Gap(frostui.SpaceXS).AlignItems(ui.Stretch).Children(func() {
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Padding(0, 0, frostui.SpaceXS, 0).Children(func() {
				frostui.Icon(c, "pocket-session").TextColor(c.Theme().Accent)
				frostui.Strong(c, g.Title).SingleLine().Shrink(1)
				if g.Tag != "" {
					frostui.Caption(c, g.Tag).SingleLine()
				}
			})
			for _, h := range g.Hits {
				a.hitRow(c, g, h, now)
			}
		})
	}
}

// hitRow is one hit: its snippet with its matches marked, and where and
// when it was said; a click opens its session at its entry.
func (a *App) hitRow(c *ui.Context, g state.SearchGroup, h proto.SearchHit, now time.Time) {
	t := c.Theme()
	label := g.Title + ": " + h.Snippet
	b := ui.ButtonBase(c.Key(h.EntryID)).Column().Gap(2).AlignItems(ui.Stretch).Padding(frostui.SpaceXS, frostui.SpaceS).Radius(6).
		Cursor(ui.CursorPointer).Label(label).FillWidth()
	if b.Hovered() {
		b.Background(t.SurfaceHover)
	}
	if b.Pressed() {
		b.Opacity(0.6)
	}
	b.Children(func() {
		var spans []ui.Span
		for _, sp := range state.SnippetSpans(h.Snippet, h.Marks) {
			if sp.Marked {
				spans = append(spans, ui.Span{Text: sp.Text, Weight: 600, Background: t.Accent.Alpha(0.22), Radius: 3})
			} else {
				spans = append(spans, ui.Span{Text: sp.Text})
			}
		}
		ui.RichText(c, spans...).Wrap()
		caption := state.HitField(h.Field)
		if !h.At.IsZero() {
			caption += " · " + state.ClockAt(h.At, now)
		}
		frostui.Caption(c, caption).SingleLine()
	})
	if b.Clicked() {
		a.openHit(g.Session, h.EntryID)
	}
}

// flashRow paints row e, the entry a hit opened, with the highlight that
// holds for flashHold and then fades, and lets it go once it faded.
func (a *App) flashRow(c *ui.Context, e ui.Element, key string) {
	tr := &a.tr
	if key != tr.flash {
		return
	}
	target := c.Theme().Accent.Alpha(0.16)
	if tr.flashed {
		target = target.Alpha(0)
	} else {
		tr.flashed = true
		c.Invalidate()
	}
	e.Background(e.AnimateColor("flash", target, ui.Animation{Duration: flashFade, Delay: flashHold, NonMotion: true})).Radius(6)
	if e.Arrived("flash") {
		tr.flash, tr.flashed = "", false
	}
}
