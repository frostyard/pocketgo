package nsgoclient

import (
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/base/textview"
	"github.com/nsgo-dev/nsgo/ui"
)

func markdownView(doc func() textview.Document, search *textview.Search) ui.TextView {
	return ui.TextView{DocumentFunc: doc, Selectable: true, Search: search, CodeBlockActions: []textview.CodeAction{{ID: textview.CopyAction}}}
}

// Link inline code that the service recognizes under an advertised home.
func (c *client) markdown(text func() string, search *textview.Search) ui.TextView {
	view := markdownView(func() textview.Document {
		doc := textview.Parse(text())
		linkBlocks(doc.Blocks, c.store.Data.FileLinkOf)
		for _, f := range doc.Footnotes {
			linkBlocks(f.Children, c.store.Data.FileLinkOf)
		}
		return doc
	}, search)
	view.OnLinkPress = func(e textview.LinkPressEvent) {
		root, path, ok := state.ParseFileLink(e.URL)
		if !ok {
			if href, found := c.store.Data.FileLinkOf(e.URL); found {
				root, path, ok = state.ParseFileLink(href)
			}
		}
		if ok {
			c.goTo(destination{page: "Files", file: state.FileSpot{Root: root, Path: path}, lookup: true})
		} else if state.SafeLink(e.URL) {
			c.cx.OpenURL(e.URL, nil)
		}
	}
	return view
}
func linkBlocks(blocks []textview.Block, lookup func(string) (string, bool)) {
	for _, block := range blocks {
		switch b := block.(type) {
		case textview.Paragraph:
			linkInlines(b.Content, lookup)
		case textview.Heading:
			linkInlines(b.Content, lookup)
		case textview.Blockquote:
			linkBlocks(b.Children, lookup)
		case textview.List:
			for _, item := range b.Items {
				linkBlocks(item.Children, lookup)
			}
		case textview.Table:
			for _, cell := range b.Head {
				linkInlines(cell.Content, lookup)
			}
			for _, row := range b.Rows {
				for _, cell := range row {
					linkInlines(cell.Content, lookup)
				}
			}
		}
	}
}
func linkInlines(in []textview.Inline, lookup func(string) (string, bool)) {
	for i, node := range in {
		switch v := node.(type) {
		case textview.Code:
			if href, ok := lookup(v.Code); ok {
				in[i] = textview.Link{URL: href, Content: []textview.Inline{v}}
			}
		case textview.Strong:
			linkInlines(v.Content, lookup)
		case textview.Emphasis:
			linkInlines(v.Content, lookup)
		case textview.Strikethrough:
			linkInlines(v.Content, lookup)
		}
	}
}
