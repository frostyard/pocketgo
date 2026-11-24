package nsgoclient

import (
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
	if p.Ready {
		add("Ready for human review", ui.BadgeAccent)
	}
	return badges
}

func (c *client) pull(p proto.PullRequest) core.Node {
	colors := ui.ColorsOf(c.cx)
	ref, valid := state.MergeRef(p)
	identity := core.View{Style: core.Style{Direction: core.Row, AlignItems: core.AlignCenter, Gap: 8, FlexGrow: 1, FlexShrink: 1, MinWidth: core.Pt(145)}, Children: []core.Node{
		ui.Icon(lucide.GitPullRequest, 18, colors.Accent), core.Text{Content: state.PullRef(p), Selectable: core.SelectableOn, Style: ui.TypeLabel.Merge(core.Style{FlexShrink: 1}), StyleFunc: ui.Colored(colors.Muted)},
	}}
	header := workspaceui.Row(identity, workspaceui.Row(pullBadges(p)...))
	out := []core.Node{header}
	if p.Title != "" {
		out = append(out, core.Text{Content: p.Title, Style: ui.TypeHeading, Selectable: core.SelectableOn})
	}
	var meta []core.Node
	if p.Base != "" {
		label := "Into " + p.Base
		if p.Head != "" {
			label = p.Head + " → " + p.Base
		}
		meta = append(meta, workspaceui.Row(ui.Icon(lucide.GitBranch, 14, colors.Muted), core.Text{Content: label, Style: ui.TypeCaption.Merge(core.Style{FlexShrink: 1}), Selectable: core.SelectableOn, StyleFunc: ui.Colored(colors.Muted)}))
	}
	if p.HeadSHA != "" {
		meta = append(meta, ui.Tooltip{Text: "Copy commit " + p.HeadSHA, Trigger: ui.Button{Icon: lucide.GitCommitHorizontal, Text: state.ShortRev(p.HeadSHA), Label: "Copy full commit SHA", Variant: ui.ButtonGhost, Size: ui.SizeSmall, OnPress: func() { c.cx.SetClipboard(p.HeadSHA) }}})
	}
	if valid {
		meta = append(meta, core.View{Style: core.Style{FlexGrow: 1}}, ui.Button{Text: "Open pull request", Icon: lucide.ExternalLink, Variant: ui.ButtonTinted, Size: ui.SizeSmall, OnPress: func() {
			url, _ := state.PullURL(p)
			c.cx.OpenURL(url, nil)
		}})
	}
	if len(meta) > 0 {
		out = append(out, workspaceui.Row(meta...))
	}
	if valid && c.gh != nil && c.gh.On() && c.gh.Status().SignedIn {
		out = append(out, c.pullMergeActions(p, ref))
	}
	return workspaceui.Card{Children: out}
}
