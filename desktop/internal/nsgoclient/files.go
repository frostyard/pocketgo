package nsgoclient

import (
	"fmt"
	"path"
	"reflect"
	"strings"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/desktop/workspaceui"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/base/textview"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

type fileScope struct{ root, base, folder, note string }
type directory struct {
	entries []proto.FileEntry
	note    string
	loaded  bool
}
type explorer struct {
	c                 *client
	cx                *core.Scope
	source            fileScope
	selection         *ui.Selection
	current           state.FileSpot
	lookup            bool
	folders           map[string]*directory
	entries           map[string]proto.FileEntry
	expanded          map[string]bool
	treeVersion       *core.Signal[int]
	selected, message *core.Signal[string]
	preview           *core.Signal[*proto.FileContent]
	request           int
	route             func(state.FileSpot, bool)
}

func (c *client) files(cx *core.Scope) core.Node {
	homes := core.NewMemoFunc(cx, func() state.FileRootsView { c.revision.Get(); return c.store.Data.FileRoots() }, func(a, b state.FileRootsView) bool { return reflect.DeepEqual(a, b) })
	source := core.NewMemo(cx, func() fileScope {
		rr := homes.Get()
		id := c.fileRoute.Get().file.Root
		for _, r := range rr.Roots.Roots {
			if id == "" || r.ID == id {
				return fileScope{root: r.ID, folder: r.Path}
			}
		}
		if rr.Err != "" {
			return fileScope{note: rr.Err}
		}
		if rr.Status != state.ListLoaded {
			return fileScope{note: "Loading agent homes…"}
		}
		if id != "" {
			return fileScope{note: "This agent home is unavailable."}
		}
		return fileScope{note: "No agent homes available."}
	})
	header := core.Dynamic(func(*core.Scope) core.Node {
		roots := homes.Get().Roots.Roots
		options := make([]ui.Option[string], 0, len(roots))
		for _, r := range roots {
			options = append(options, ui.Option[string]{Value: r.ID, Label: r.Name})
		}
		if len(options) == 0 {
			return core.Group{}
		}
		val := func() string { return source.Get().root }
		change := func(id string) { c.goTo(destination{page: "Files", file: state.FileSpot{Root: id}}) }
		var picker core.Node = ui.Tabs[string]{Label: "Agent homes", Variant: ui.TabsUnderline, Options: options, Value: val, OnChange: change}
		if len(options) > 4 || cx.Env().Window().Width < 620 {
			picker = ui.Select[string]{Label: "Agent home", Options: options, Value: val, OnChange: change}
		}
		return workspaceui.Row(picker)
	})
	return core.View{Style: ui.FillRest, Children: []core.Node{header, core.Dynamic(func(cx *core.Scope) core.Node {
		src := source.Get()
		return c.explorer(cx, src, false)
	})}}
}

func (c *client) sessionFiles(cx *core.Scope) core.Node {
	source := core.NewMemo(cx, func() fileScope {
		c.revision.Get()
		id := c.session.Get()
		s, ok := c.store.Data.Session(id)
		if !ok || s.Cwd == "" {
			return fileScope{note: "This session has no working folder."}
		}
		rr := c.store.Data.FileRoots()
		if rr.Status != state.ListLoaded && rr.Err == "" {
			return fileScope{note: "Loading session folder…"}
		}
		if rr.Roots.SessionFolders {
			return fileScope{root: proto.SessionFilesRoot(id), folder: s.Cwd}
		}
		if root, base, ok := state.AbsoluteFileSpot(rr.Roots, s.Cwd); ok {
			return fileScope{root: root, base: base, folder: s.Cwd}
		}
		if rr.Err != "" {
			return fileScope{note: rr.Err}
		}
		return fileScope{note: "Update pocketd to browse " + s.Cwd + " outside the advertised agent homes."}
	})
	return core.Dynamic(func(cx *core.Scope) core.Node {
		src := source.Get()
		c.session.Get()
		return c.explorer(cx, src, true)
	})
}

func (c *client) explorer(cx *core.Scope, src fileScope, session bool) core.Node {
	if src.root == "" {
		return workspaceui.Page(body(src.note))
	}
	e := &explorer{
		c: c, cx: cx, source: src, selection: ui.NewSelection(cx, false),
		folders: make(map[string]*directory), entries: make(map[string]proto.FileEntry), expanded: make(map[string]bool),
		treeVersion: core.NewSignal(cx, 0), selected: core.NewSignal(cx, ""), message: core.NewSignal(cx, ""),
		preview: core.NewSignal(cx, (*proto.FileContent)(nil)),
	}
	e.load("")
	e.route = func(spot state.FileSpot, lookup bool) {
		if session {
			e.show(spot, lookup)
			return
		}
		c.goTo(destination{page: "Files", file: spot, lookup: lookup})
	}
	if !session {
		cx.Effect(func() {
			d := c.fileRoute.Get()
			cx.Untrack(func() { e.show(d.file, d.lookup) })
		})
	}
	lastRefresh := c.fileRefresh.GetUntracked()
	cx.Effect(func() {
		next := c.fileRefresh.Get()
		if lastRefresh == next {
			return
		}
		lastRefresh = next
		cx.Untrack(func() {
			clear(e.folders)
			clear(e.entries)
			e.load("")
			for dir, open := range e.expanded {
				if open {
					e.load(dir)
				}
			}
			e.show(e.current, e.lookup)
		})
	})
	width := core.Points(235)
	if session {
		width = 170
	}
	header := core.View{Style: core.Style{Padding: core.Symmetric(8, 12), Gap: 6, Direction: core.Row, AlignItems: core.AlignCenter}, Children: []core.Node{ui.Icon(lucide.FolderOpen, 16, ui.Inherited), core.Text{Content: src.folder, Style: ui.TypeCaption, MaxLines: 1, Selectable: core.SelectableOn}}}
	return workspaceui.FileExplorer{Header: header, Tree: e.tree(), Preview: core.Dynamic(e.content), TreeWidth: width, HasPreview: func() bool { return e.selected.Get() != "" }, BackToTree: func() { e.route(state.FileSpot{Root: src.root}, false) }}
}

func (e *explorer) changed() { e.treeVersion.Update(func(i int) int { return i + 1 }) }

func (e *explorer) load(dir string) {
	if e.folders[dir] != nil {
		return
	}
	d := &directory{}
	e.folders[dir] = d
	e.c.store.BrowseFolder(e.source.root, state.JoinFilePath(e.source.base, dir), func(f proto.FileFolder, err error) {
		if e.cx.Disposed() || e.folders[dir] != d {
			return
		}
		d.loaded = true
		if err != nil {
			d.note = err.Error()
			if dir == "" {
				e.message.Set(d.note)
			}
		} else {
			d.entries = state.SortFileEntries(f.Entries)
			for _, item := range d.entries {
				e.entries[state.JoinFilePath(dir, item.Name)] = item
			}
			d.note = strings.Join(state.FolderNotes(f), " ")
			if len(d.entries) == 0 && d.note == "" {
				d.note = "Empty folder"
			}
		}
		e.changed()
	})
}

func (e *explorer) show(spot state.FileSpot, lookup bool) {
	e.current, e.lookup = spot, lookup
	e.request++
	ticket := e.request
	e.preview.Set(nil)
	e.message.Set("")
	e.selected.Set("")
	if spot.Path == "" {
		e.selection.Clear()
		return
	}
	p := spot.Path
	if _, err := proto.CheckFilePath(p); err != nil {
		e.message.Set(err.Error())
		return
	}
	dir := state.ParentPath(p)
	for parent := dir; parent != ""; parent = state.ParentPath(parent) {
		e.expanded[parent] = true
		e.load(parent)
	}
	e.changed()
	if lookup {
		e.c.store.BrowseFolder(e.source.root, state.JoinFilePath(e.source.base, dir), func(f proto.FileFolder, err error) {
			if e.cx.Disposed() || ticket != e.request {
				return
			}
			if err != nil {
				e.message.Set(err.Error())
				return
			}
			for _, item := range f.Entries {
				if item.Name == path.Base(p) && state.EntryOpens(item) {
					e.show(state.FileSpot{Root: spot.Root, Path: p, File: item.Kind == proto.FileEntryFile}, false)
					return
				}
			}
			e.message.Set("This entry is absent or withheld from the listing.")
		})
		return
	}
	if !spot.File {
		e.expanded[p] = true
		e.load(p)
		e.changed()
		return
	}
	e.selected.Set(p)
	e.selection.Set(p)
	e.message.Set("Loading preview…")
	e.c.store.BrowseFile(e.source.root, state.JoinFilePath(e.source.base, p), func(f proto.FileContent, err error) {
		if e.cx.Disposed() || ticket != e.request {
			return
		}
		if err != nil {
			e.message.Set(err.Error())
			return
		}
		e.message.Set("")
		e.preview.Set(&f)
	})
}

const folderHint = "\x00"

func (e *explorer) children(dir string) []any {
	e.treeVersion.Get()
	d := e.folders[dir]
	if d == nil || !d.loaded {
		return []any{dir + folderHint}
	}
	keys := make([]any, 0, len(d.entries)+1)
	for _, item := range d.entries {
		keys = append(keys, state.JoinFilePath(dir, item.Name))
	}
	if d.note != "" {
		keys = append(keys, dir+folderHint)
	}
	return keys
}

func (e *explorer) label(key any) string {
	p := key.(string)
	if dir, ok := strings.CutSuffix(p, folderHint); ok {
		if d := e.folders[dir]; d != nil && d.loaded {
			return d.note
		}
		return "Loading folder…"
	}
	item := e.entries[p]
	if !state.EntryOpens(item) {
		return item.Name + " · " + state.EntryCaption(item)
	}
	return item.Name
}

func (e *explorer) tree() core.Node {
	sel := e.selection
	expand := func(key any, v bool) {
		p := key.(string)
		e.expanded[p] = v
		if v {
			e.load(p)
		}
		e.changed()
	}
	choose := func(key any) {
		p, ok := key.(string)
		if !ok || !sel.Has(key) {
			return
		}
		item := e.entries[p]
		if !state.EntryOpens(item) {
			return
		}
		if item.Kind == proto.FileEntryFolder {
			if !e.expanded[p] {
				expand(key, true)
			}
			return
		}
		if e.selected.GetUntracked() != p {
			e.route(state.FileSpot{Root: e.source.root, Path: p, File: true}, false)
		}
	}
	e.cx.Effect(func() { key := sel.Cursor(); e.cx.Untrack(func() { choose(key) }) })
	return ui.Tree{Label: "File tree", TestID: "files-tree", Style: ui.FillRest, ToggleLabel: "Expand or collapse folder", Selection: sel,
		Roots: func() []any { return e.children("") }, Children: func(k any) []any {
			p := k.(string)
			if item := e.entries[p]; item.Kind == proto.FileEntryFolder && state.EntryOpens(item) {
				return e.children(p)
			}
			return nil
		},
		Expanded: func(k any) bool { return e.expanded[k.(string)] }, OnExpand: expand, RowLabel: e.label,
		Row: func(cx *core.Scope, it ui.TreeItem) core.Node {
			return core.Dynamic(func(*core.Scope) core.Node {
				key, ok := it.Key().(string)
				if !ok {
					return core.Group{}
				}
				icon := lucide.FileText
				if strings.HasSuffix(key, folderHint) {
					return core.Text{ContentFunc: func() string { e.treeVersion.Get(); return e.label(key) }, Style: ui.TypeCaption}
				}
				item := e.entries[key]
				if item.Kind == proto.FileEntryFolder {
					icon = lucide.Folder
				}
				if item.Withheld {
					icon = lucide.Lock
				}
				return core.View{Style: core.Style{Direction: core.Row, Gap: 6, AlignItems: core.AlignCenter, FlexShrink: 1}, Children: []core.Node{ui.Icon(icon, 15, ui.Inherited), core.Text{Content: e.label(key), MaxLines: 1, Style: ui.TypeCaption}}}
			})
		},
	}
}

func (e *explorer) content(cx *core.Scope) core.Node {
	p, msg, f := e.selected.Get(), e.message.Get(), e.preview.Get()
	if p == "" && msg == "" {
		msg = "Select a file to preview"
	}
	children := []core.Node{}
	if p != "" {
		children = append(children, core.Text{Content: p, Style: ui.TypeTitle, Selectable: core.SelectableOn})
	}
	if msg != "" {
		children = append(children, body(msg))
	}
	if f != nil {
		if note := state.FileNote(*f); note != "" {
			children = append(children, body(note))
		}
		switch state.FileShows(*f) {
		case proto.FileMarkdown:
			view := markdownView(func() textview.Document { return textview.Parse(f.Text) }, nil)
			view.OnLinkPress = func(ev textview.LinkPressEvent) {
				root, rel, ok := state.ResolveFileHref(e.c.store.Data.FileRoots().Roots, state.FileSpot{Root: e.source.root, Path: state.JoinFilePath(e.source.base, p), File: true}, ev.URL)
				if !ok {
					if state.SafeLink(ev.URL) {
						cx.OpenURL(ev.URL, nil)
					}
					return
				}
				if root == e.source.root && (e.source.base == "" || strings.HasPrefix(rel, e.source.base+"/")) {
					e.route(state.FileSpot{Root: root, Path: strings.TrimPrefix(rel, e.source.base+"/")}, true)
					return
				}
				e.c.goTo(destination{page: "Files", file: state.FileSpot{Root: root, Path: rel}, lookup: true})
			}
			children = append(children, view)
		case proto.FileText:
			children = append(children, core.Text{Content: f.Text, Selectable: core.SelectableOn, Style: ui.TypeCode})
		case proto.FileImage:
			children = append(children, core.Image{Source: core.ImageSource{Data: f.Image}, Style: core.Style{Height: core.Pt(360), Width: core.Pct(100)}})
		}
		children = append(children, core.Text{Content: fmt.Sprintf("%s · %s", f.Kind, state.FileSize(f.Size)), Style: ui.TypeCaption})
	}
	return core.Scroll{Style: ui.FillRest, Children: []core.Node{core.View{Style: core.Style{Padding: core.All(14), Gap: 10}, Children: children}}}
}
