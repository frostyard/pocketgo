package nsgoclient

import (
	"fmt"

	"github.com/frostyard/frostui/workspaceui"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
	"github.com/nsgo-dev/nsgo/core"
	"github.com/nsgo-dev/nsgo/icons/lucide"
	"github.com/nsgo-dev/nsgo/ui"
)

func pullBadges(p proto.PullRequest) []core.Node {
	var badges []core.Node
	add := func(label string, variant ui.BadgeVariant) {
		badges = append(badges, ui.Badge{Text: label, Variant: variant})
	}
	switch p.State {
	case proto.PullOpen:
		if p.Draft {
			add("Draft", ui.BadgeNeutral)
		} else {
			add("Open", ui.BadgeSuccess)
		}
	case proto.PullMerged:
		add("Merged", ui.BadgeAccent)
	case proto.PullClosed:
		add("Closed", ui.BadgeNeutral)
	}
	switch p.Checks {
	case proto.ChecksPending:
		add("Checks pending", ui.BadgeWarn)
	case proto.ChecksFailing:
		add("Checks failing", ui.BadgeDanger)
	case proto.ChecksGreen:
		add("Checks passed", ui.BadgeSuccess)
	case proto.ChecksNone:
		add("No checks", ui.BadgeNeutral)
	}
	// The label as GitHub has it, and what pocketd last read behind it.
	if p.Ready {
		badges = append(badges, ui.Tooltip{Text: readyTooltip(p), Trigger: ui.Badge{Text: "ready-for-human", Variant: ui.BadgeSuccess}})
	}
	return badges
}

// readyTooltip says what the ready-for-human pill shows: the label as
// GitHub has it, and what pocketd last read of its review and CI
// (state.ReadyBacked), as past reads (ADR-0017).
func readyTooltip(p proto.PullRequest) string {
	text := "It has ready-for-human on GitHub."
	if state.ReadyBacked(p) {
		return text + " The newest cross-review at its head said ready, and CI was green when pocketd last read it."
	}
	return text + " pocketd has not read a ready cross-review at its head with green CI."
}

// reviewLine says what the newest cross-review at p's head found, and
// CI's state there (ADR-0017): "" when no verdict was filed at its head.
func (c *client) reviewLine(p proto.PullRequest) string {
	r := p.Review
	if r == nil {
		return ""
	}
	head := r.Head
	if len(head) > 12 {
		head = head[:12]
	}
	line := fmt.Sprintf("Cross-review R%d by %s (%s): %s at %s", r.ID, c.store.Data.AgentName(r.Agent), r.Model.ID, r.Verdict, head)
	if p.Checks != "" {
		line += " · CI " + string(p.Checks)
	}
	if r.Verdict != proto.ReviewReady && len(r.Findings) > 0 {
		line += " · " + r.Findings[0]
	}
	return line
}

func (c *client) pull(p proto.PullRequest) core.Node {
	colors := ui.ColorsOf(c.cx)
	ref, valid := state.MergeRef(p)
	identity := core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8, FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(145)}, Children: []core.Node{
		ui.Icon(lucide.GitPullRequest, 18, colors.Accent), core.Text{Content: state.PullRef(p), Selectable: core.SelectableOn, Style: ui.TypeLabel.Merge(core.Style{FlexShrink: 1}), StyleFunc: ui.Colored(colors.Muted)},
	}}
	// The pills stand in the row itself: in a narrow card, a lane's, they
	// wrap under the pull request's name.
	header := workspaceui.Row(append([]core.Node{identity}, pullBadges(p)...)...)
	out := []core.Node{header}
	if p.Title != "" {
		out = append(out, core.Text{Content: p.Title, Style: ui.TypeHeading, Selectable: core.SelectableOn})
	}
	if line := c.reviewLine(p); line != "" {
		out = append(out, c.caption(line))
	}
	var meta []core.Node
	if p.Base != "" {
		label := "Into " + p.Base
		if p.Head != "" {
			label = p.Head + " → " + p.Base
		}
		// The branches give way in a narrow card, a lane's: they wrap
		// inside it.
		meta = append(meta, core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: workspaceui.SpaceS, FlexShrink: 1, MinWidth: core.Pt(0)}, Children: []core.Node{
			ui.Icon(lucide.GitBranch, 14, colors.Muted),
			core.Text{Content: label, Style: ui.TypeCaption.Merge(core.Style{FlexShrink: 1, MinWidth: core.Pt(0)}), Selectable: core.SelectableOn, StyleFunc: ui.Colored(colors.Muted)}}})
	}
	if p.HeadSHA != "" {
		meta = append(meta, ui.Tooltip{Text: "Copy commit " + p.HeadSHA, Trigger: ui.Button{Icon: lucide.GitCommitHorizontal, Text: state.ShortRev(p.HeadSHA), Label: "Copy full commit SHA", Variant: ui.ButtonGhost, Size: ui.SizeSmall, OnPress: func() { c.cx.SetClipboard(p.HeadSHA) }}})
	}
	// It opens on GitHub at the link the client builds, or not at all.
	if url, ok := state.PullURL(p); ok {
		meta = append(meta, core.View{Style: core.Style{FlexGrow: 1}}, ui.Button{Text: "Open pull request", Icon: lucide.ExternalLink, Variant: ui.ButtonTinted, Size: ui.SizeSmall, OnPress: func() {
			c.cx.OpenURL(url, nil)
		}})
	} else {
		meta = append(meta, c.caption("no GitHub link"))
	}
	if len(meta) > 0 {
		out = append(out, workspaceui.Row(meta...))
	}
	// Its approve and merge, or why they are off: the controls show only
	// where the GitHub sign-in holds (pullMerge).
	if valid && c.gh != nil {
		out = append(out, c.pullMerge(p, ref))
	}
	return workspaceui.Card{Children: out}
}
