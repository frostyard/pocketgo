package app

import (
	"fmt"
	"strconv"

	"github.com/egoist/mygo/ui"
	"github.com/frostyard/frostui"

	"github.com/frostyard/pocketgo/desktop/internal/state"
	"github.com/frostyard/pocketgo/proto"
)

// workerStrip draws a session's worker cards, or a worker's way back to its specialist.
func (a *App) workerStrip(c *ui.Context, id int64) {
	t := c.Theme()
	cp := &a.composer
	if h, ok := a.st.Data.WorkerHeaderOf(id); ok {
		ui.Row(c.Key("worker-header")).Wrap().Gap(frostui.SpaceS).AlignItems(ui.Center).Children(func() {
			if h.Parent != 0 {
				b := ui.ButtonBase(c.Key("back-to-parent")).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).
					MinHeight(32).Cursor(ui.CursorPointer).Label("Back to the specialist's session, " + h.ParentTitle).Tooltip(h.ParentTitle)
				if a.compact {
					b.MinHeight(44)
				}
				if b.Pressed() {
					b.Opacity(0.4)
				}
				b.Children(func() {
					frostui.Icon(c, "go-previous").TextColor(t.Accent)
					ui.Text(c, "Back to specialist").TextColor(t.Accent).SingleLine().Shrink(0)
					frostui.Caption(c, h.ParentTitle).SingleLine().MaxWidth(240).Shrink(1).MinWidth(0)
				})
				if b.Clicked() {
					a.openSession(h.Parent)
				}
			}
			ui.Row(c.Key("worker-self")).Gap(frostui.SpaceXS).AlignItems(ui.Center).Shrink(1).MinWidth(0).Children(func() {
				frostui.Caption(c, "Worker:")
				frostui.Caption(c, h.Title).TextColor(t.Text).Bold().SingleLine().MaxWidth(240).Shrink(1).MinWidth(0)
				if name := state.ModelName(h.Model); name != "" {
					frostui.Caption(c, name).Tooltip(h.Model.String())
				}
			})
		})
	}
	cards := a.st.Data.WorkerCards(id, cp.workersOpen[id])
	if len(cards.Cards) == 0 {
		return
	}
	ui.Row(c.Key("worker-cards")).Wrap().Gap(frostui.SpaceXS).AlignItems(ui.Center).Children(func() {
		for _, card := range cards.Cards {
			tooltip := card.Title
			if state.ModelName(card.Model) != "" {
				tooltip += " · " + card.Model.String()
			}
			b := ui.ButtonBase(c.Key(fmt.Sprint("worker-card:", card.ID))).Row().Gap(frostui.SpaceXS).AlignItems(ui.Center).
				Padding(4, 8).Radius(6).Background(t.Surface).BorderWidth(1, 1, 1, 1).BorderColor(t.Border).
				MaxWidth(340).Shrink(1).MinWidth(0).Cursor(ui.CursorPointer).Label(state.WorkerCardLabel(card)).Tooltip(tooltip)
			if a.compact {
				b.MinHeight(36)
			}
			if b.Pressed() {
				b.Opacity(0.4)
			}
			b.Children(func() {
				tone := frostui.Neutral
				switch card.State {
				case proto.SessionRunning:
					tone = frostui.Accent
				case proto.SessionWaiting:
					tone = frostui.Warning
				}
				ui.Box(c).Size(8, 8).Shrink(0).Radius(4).Background(frostui.ToneColor(t, tone))
				frostui.Caption(c, card.Title).TextColor(t.Text).SingleLine().Shrink(1).MinWidth(0)
				if name := state.ModelName(card.Model); name != "" {
					frostui.Caption(c, name).SingleLine().Shrink(0)
				}
				frostui.Caption(c, string(card.State)).SingleLine().Shrink(0)
			})
			if b.Clicked() {
				a.openSession(card.ID)
			}
		}
		if cards.More > 0 {
			if ui.Button(c.Key("worker-cards-more"), state.MoreWorkersLabel(cards.More)).Label("Show " + strconv.Itoa(cards.More) + " more workers").Clicked() {
				if cp.workersOpen == nil {
					cp.workersOpen = map[int64]bool{}
				}
				cp.workersOpen[id] = true
			}
		}
		if cards.Fewer {
			if ui.Button(c.Key("worker-cards-fewer"), "Show fewer").Label("Show fewer workers").Clicked() {
				if cp.workersOpen == nil {
					cp.workersOpen = map[int64]bool{}
				}
				cp.workersOpen[id] = false
			}
		}
	})
}
