package nsgoclient

import (
	"github.com/frostyard/pocketgo/proto"
	"slices"
	"strings"
	"time"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/nsgo-dev/nsgo/base/textview"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/ui"
	"github.com/nsgo-dev/nsgo/ui/chat"
)

// Each Markdown message mounts a tree of native controls. Keep the work of
// opening a session bounded as its history grows.
const transcriptPageSize = 36

// The highlight of the entry a search hit opens: shown at once, held,
// then gone in a fade, in a lighter tint than a hit's marked words.
const (
	flashHold = 1200 * time.Millisecond
	flashFade = 600 * time.Millisecond
	flashTint = 0.14
)

func (c *client) transcript(cx *core.Scope, id int64, rows func() []state.Row, view *chat.ConversationState) core.Node {
	// An empty anchor follows incoming messages. A historical page ends at a
	// row key so streaming and new messages cannot move the reader's place.
	endKey := core.NewSignal(cx, "")
	target := core.NewSignal(cx, "")
	isolate := core.NewSignal(cx, "")
	// flash is the row a search hit opened: it shows highlighted at once,
	// and the highlight goes in a moment (flashHold, then a fade), as in
	// the MyGo client.
	flash := core.NewSignal(cx, "")
	cx.Effect(func() {
		// A newer highlight starts its own moment: the effect's next run
		// ends this one's timer.
		if flash.Get() != "" {
			cx.AfterFunc(flashHold, func() { flash.Set("") })
		}
	})
	cx.Effect(func() {
		entry := c.seek.Get()
		if entry == 0 {
			return
		}
		all := rows()
		if i, ok := state.TargetRow(all, entry); ok {
			target.Set(all[i].Key)
			isolate.Set(all[i].Key)
			flash.Set(all[i].Key)
			if len(all) > transcriptPageSize {
				endKey.Set(all[i].Key)
			}
			c.seek.Set(0)
		}
	})
	query := core.NewMemo(cx, func() string {
		if view.SearchOpen() && len(rows()) > transcriptPageSize {
			return strings.ToLower(strings.TrimSpace(view.Search().Query()))
		}
		return ""
	})
	// Find searches the whole conversation, including pages that are not
	// mounted. The gallery find bar highlights occurrences on the result page.
	type indexedText struct{ source, plain string }
	var plainText map[string]indexedText
	hits := core.NewMemoFunc(cx, func() []state.Row {
		all := rows()
		q := query.Get()
		if q == "" {
			plainText = nil
			return all
		}
		if plainText == nil {
			plainText = make(map[string]indexedText)
		}
		var found []state.Row
		for _, r := range all {
			if transcriptMatches(r, q) {
				found = append(found, r)
				continue
			}
			if r.Text == "" || (r.Kind != state.RowAssistant && r.Kind != state.RowUser) {
				continue
			}
			doc, ok := plainText[r.Key]
			if !ok || doc.source != r.Text {
				doc = indexedText{r.Text, strings.ToLower(textview.Parse(r.Text).PlainText())}
				plainText[r.Key] = doc
			}
			if strings.Contains(doc.plain, q) {
				found = append(found, r)
			}
		}
		return found
	}, nil)
	type findPage struct{ query, key string }
	findEnd := core.NewSignal(cx, findPage{})
	end := core.NewMemo(cx, func() int {
		items := hits.Get()
		key := endKey.Get()
		if q := query.Get(); q != "" {
			p := findEnd.Get()
			if q != p.query || p.key == "" {
				return min(transcriptPageSize, len(items))
			}
			key = p.key
		}
		if key != "" {
			i := slices.IndexFunc(items, func(r state.Row) bool { return r.Key == key })
			if i >= 0 {
				return i + 1
			}
		}
		return len(items)
	})
	turn := func(delta int) {
		items := hits.Get()
		next := min(len(items), max(transcriptPageSize, end.Get()+delta))
		if next == 0 {
			return
		}
		key := items[next-1].Key
		if q := query.Get(); q != "" {
			findEnd.Set(findPage{q, key})
		} else if next == len(items) {
			endKey.Set("")
		} else {
			endKey.Set(key)
		}
		view.JumpToLatest()
	}
	previous := core.Show{When: func() bool { return end.Get() > transcriptPageSize }, Then: func(*core.Scope) core.Node {
		return c.button("Earlier messages", func() { turn(-transcriptPageSize) })
	}}
	next := core.Show{When: func() bool { return end.Get() < len(hits.Get()) }, Then: func(*core.Scope) core.Node {
		return workspaceui.Row(c.button("Later messages", func() { turn(transcriptPageSize) }), c.button("Latest messages", func() { turn(len(hits.Get())) }))
	}}
	page := func() []state.Row {
		n := end.Get()
		return hits.Get()[max(0, n-transcriptPageSize):n]
	}
	// Group only the visible page. An explicit search result stays a standalone
	// entry until the reader moves on, so even a folded tool can be revealed.
	messages := workspaceui.ConversationEntries[turnPart]{
		Items: func() []turnPart {
			if query.Get() != "" {
				// Filtered matches can span unrelated turns.
				var matches []turnPart
				for _, r := range page() {
					matches = append(matches, turnPart{key: r.Key, rows: []state.Row{r}})
				}
				return matches
			}
			return groupWork(page(), isolate.Get())
		},
		Key: func(p turnPart) string { return p.key },
		Content: func(cx *core.Scope, p func() turnPart) core.Node {
			var row core.Node
			if core.Untracked(cx, p).work {
				row = c.workTurn(cx, p, view)
			} else {
				row = c.message(cx, func() state.Row { return p().rows[0] }, view)
			}
			key := core.Untracked(cx, p).key
			colors := ui.ColorsOf(cx)
			return core.View{TestID: "transcript-row-" + key, Style: core.Style{CornerRadius: ui.RadiusInset, Transition: core.Transition{Duration: flashFade, Easing: core.EaseOut, Properties: core.TransitionBackground}},
				StyleFunc: func() core.Style {
					if flash.Get() != key {
						return core.Style{}
					}
					return core.Style{Background: ui.Mix(colors.Surface(), colors.Accent(), flashTint)}
				},
				Children: []core.Node{row}}
		}, Reveal: target.Get, OnRevealed: func(string) { target.Set("") },
	}
	// Keep pending approvals actionable even when the originating tool is
	// outside the mounted page or its entry has not reached the client yet.
	offPage := workspaceui.ConversationEntries[proto.Approval]{
		Items: func() []proto.Approval {
			c.revision.Get()
			pending := c.store.Data.ApprovalsFor(id)
			if len(pending) == 0 {
				return nil
			}
			shown := map[string]bool{}
			for _, r := range page() {
				if r.Tool != nil {
					shown[r.Tool.ApprovalID] = true
				}
			}
			var missing []proto.Approval
			for _, a := range pending {
				if !shown[a.ID] {
					missing = append(missing, a)
				}
			}
			return missing
		},
		Key: func(a proto.Approval) string { return a.ID },
		Content: func(cx *core.Scope, approval func() proto.Approval) core.Node {
			a := core.Untracked(cx, approval)
			return workspaceui.Card{Children: []core.Node{title("Approval · " + a.Tool), body(string(a.Arguments)), c.approvalActions(a.ID)}}
		},
	}
	return core.Group{previous, messages, next, offPage}
}

func transcriptMatches(r state.Row, query string) bool {
	contains := func(s string) bool { return strings.Contains(strings.ToLower(s), query) }
	if contains(r.Text) || contains(r.Thinking) {
		return true
	}
	if t := r.Tool; t != nil && (contains(t.Name) || contains(t.Summary) || contains(t.Arguments) || contains(t.Result)) {
		return true
	}
	if r.Question != nil && contains(r.Question.Question) {
		return true
	}
	for _, f := range r.Files {
		if contains(f.Name) || contains(f.Path) {
			return true
		}
	}
	return false
}
