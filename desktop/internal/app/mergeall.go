package app

import (
	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/github"
	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// mergeAllBar heads the Inbox's pull requests with Merge All Ready
// (ADR-0031): the button, then where the run stands, and how it ended.
// state.GitHub holds every rule; the bar only draws it.
func (a *App) mergeAllBar(c *ui.Context, pulls []proto.PullRequest) {
	g := a.gh
	run := g.MergeAll()
	if run.Phase == state.MergeAllNone {
		if len(pulls) == 0 || !g.On() {
			return
		}
		if phase, _, _ := g.SignIn(); phase != state.SignedIn {
			return // each card offers the sign-in
		}
	}
	t := c.Theme()
	ui.Column(c.Key("merge-all")).Gap(frostui.SpaceS).Padding(frostui.SpaceM, frostui.SpaceL).AlignItems(ui.Stretch).
		Radius(10).Background(frostui.Panel(t)).Role(ui.RoleGroup).Label("Merge every ready pull request").Children(func() {
		switch run.Phase {
		case state.MergeAllNone:
			ok, why := g.CanMergeAll(pulls)
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				text := "Approve and merge every pull request whose gates pass now, one at a time, as " + github.Login + "."
				if !ok {
					text = "Merge All Ready: " + why + "."
				}
				frostui.Caption(c, text).Wrap().Grow(1)
				tip := "Read the gates of every pull request pocketd backs, then confirm the list"
				if !ok {
					tip = why
				}
				if ui.Button(c.Key("merge-all:start"), "Merge All Ready").Disabled(!ok).Tooltip(tip).Clicked() {
					g.MergeAllReady(a.st.Data.PullsToReview)
				}
			})
		case state.MergeAllChecking:
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				frostui.Loading(c, "Reading the merge gates on GitHub: "+itoa(run.Checked)+" of "+itoa(run.Checked+run.Checking)+" read…").Grow(1)
				if ui.Button(c.Key("merge-all:stop"), "Stop").Clicked() {
					g.StopMergeAll()
				}
			})
		case state.MergeAllConfirming:
			frostui.Caption(c, "Merge All Ready waits for your confirmation of "+pullCount(len(run.Items))+".").Wrap()
		case state.MergeAllMerging:
			ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
				text := "Approving and merging " + itoa(run.Turn+1) + " of " + itoa(len(run.Items)) + " as " + github.Login
				if run.Turn < len(run.Items) {
					text += ": " + run.Items[run.Turn].Ref.String()
				}
				// An earlier merge moved its base: its gates are read again
				// there first (ADR-0038).
				if run.Rechecking {
					text += " · checking it again at its new base"
				}
				if run.Stopping {
					text += " · stopping after this one"
				}
				frostui.Loading(c, text+"…").Grow(1)
				if ui.Button(c.Key("merge-all:stop"), "Stop").Disabled(run.Stopping).Tooltip("Stop once the merge under way ends").Clicked() {
					g.StopMergeAll()
				}
			})
		case state.MergeAllEnded:
			a.mergeAllEnded(c, run)
		}
	})
}

// mergeAllEnded says how a run ended: how many merged, each that stayed in
// the Inbox or was not tried, with its reason, then each that waits for its
// dependency bot to rebase it.
func (a *App) mergeAllEnded(c *ui.Context, run state.MergeAll) {
	ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
		frostui.Strong(c, "Merge All Ready: "+run.Counts().String()).Grow(1).Wrap()
		if ui.Button(c.Key("merge-all:dismiss"), "Dismiss").Clicked() {
			a.gh.DismissMergeAll()
		}
	})
	if run.End != "" {
		frostui.Caption(c, run.End+".").Wrap()
	}
	// Each that did not merge: one in the merge queue may yet leave it, and
	// one whose end is unknown needs a check (Codex review of #53, round 1).
	// Those that wait for their bot to rebase them come after, apart, so
	// this list is of failures (the owner's choice of 2026-11-21).
	for _, it := range run.Items {
		if it.Result.Kind == "merged" || it.Rebase != "" {
			continue
		}
		ui.Row(c.Key("merge-all:item:" + it.Ref.String())).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			if !it.Tried() {
				frostui.Pill(c, "not tried", frostui.Neutral)
				frostui.Caption(c, it.Ref.String()).Wrap().Grow(1)
				return
			}
			text, tone := mergeResult(it.Result)
			name := it.Ref.String()
			if it.Rechecked {
				name += " (checked again at its new base)"
			}
			frostui.Pill(c, it.Result.Kind, tone)
			frostui.Caption(c, name+": "+text).Wrap().Grow(1)
		})
	}
	for _, it := range run.Items {
		if it.Rebase == "" {
			continue
		}
		ui.Row(c.Key("merge-all:rebase:" + it.Ref.String())).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			frostui.Pill(c, "rebase", frostui.Neutral)
			frostui.Caption(c, it.Ref.String()+": waits for "+it.Rebase+" to rebase it ("+it.Result.Reason+")").Wrap().Grow(1)
		})
	}
}

// mergeAllConfirmView is Merge All Ready's one confirmation: every pull
// request it lists, its head and method, then Cancel and the action. The
// list scrolls, and the buttons stay at the bottom.
func (a *App) mergeAllConfirmView(c *ui.Context) {
	g := a.gh
	run := g.MergeAll()
	if run.Phase != state.MergeAllConfirming {
		return
	}
	open, decided, confirmed := true, false, false
	title := "Approve and merge " + pullCount(len(run.Items)) + "?"
	ui.Modal(c, &open, func() {
		ui.Column(c).Width(560).Gap(frostui.SpaceM).AlignItems(ui.Stretch).Shrink(1).MinHeight(0).Role(ui.RoleAlertDialog).Label(title).Children(func() {
			ui.Text(c, title).FontSize(18).Bold().Wrap()
			frostui.Muted(c, "PocketGo approves each at the head shown as "+github.Login+", then merges it with the method shown, one at a time. "+
				"Right before each, it reads the pull request from pocketd again and its gates on GitHub again; one that fails stays in the Inbox "+
				"with its reason, and the rest go on. One listed without its diff opened whose base an earlier merge moved is checked again "+
				"at the new base first.").Wrap()
			ui.Scroll(c).Grow(1).Shrink(1).MinHeight(0).Label("The pull requests to merge").Children(func() {
				ui.Column(c).Gap(frostui.SpaceS).AlignItems(ui.Stretch).Children(func() {
					for _, it := range run.Items {
						p := it.Merge.Preview
						ui.Column(c.Key("merge-all:list:" + it.Ref.String())).Gap(2).AlignItems(ui.Stretch).Children(func() {
							// The title GitHub read with the head and base shown
							// (Codex review of #53, round 2).
							title := p.Title
							if title == "" {
								title = it.Pull.Title
							}
							ui.Row(c).Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
								frostui.Strong(c, it.Ref.String()).SingleLine()
								frostui.Caption(c, title).SingleLine().Grow(1)
							})
							text := "at " + shortHead(p.Head) + " into " + p.BaseRef + " · " + methodName(p)
							// A dependency bot's risk:high pull request goes
							// without its diff opened (ADR-0031).
							if it.Waived {
								text += " · risk:high, by a dependency bot: no diff opened"
							}
							if it.Note != "" {
								text += " · " + it.Note
							}
							frostui.Caption(c, text).Wrap()
						})
					}
				})
			})
			if left := run.Checked - len(run.Items); left > 0 {
				frostui.Caption(c, notListed(left)).Wrap()
			}
			// It opens when the last check ends, not at a click: the focus
			// goes to Cancel, so Enter meant for the composer never confirms
			// the list (Codex review of #53, round 2).
			ui.Row(c).Gap(frostui.SpaceS).Justify(ui.End).Children(func() {
				if ui.Button(c.Key("merge-all:cancel"), "Cancel").AutoFocus().Clicked() {
					decided = true
				}
				if ui.PrimaryButton(c.Key("merge-all:confirm"), "Approve + Merge "+itoa(len(run.Items))).Clicked() {
					decided, confirmed = true, true
				}
			})
		})
	})
	switch {
	case confirmed:
		g.ConfirmMergeAll(run.ID, a.st.ReadPull)
	case decided || !open:
		g.CancelMergeAll(run.ID)
	}
}

// pullCount is n pull requests, in words.
func pullCount(n int) string {
	if n == 1 {
		return "1 pull request"
	}
	return itoa(n) + " pull requests"
}

// notListed says that n pull requests pocketd backs were read and do not
// pass now.
func notListed(n int) string {
	if n == 1 {
		return "1 other pull request pocketd backs does not pass now: its card says why."
	}
	return itoa(n) + " other pull requests pocketd backs do not pass now: each card says why."
}
